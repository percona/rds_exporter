package enhanced

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/percona/rds_exporter/sessions"
)

// Maximal and minimal metrics update interval.
const (
	maxInterval = 60 * time.Second
	minInterval = 2 * time.Second

	// minMetricsTTL is how long a sample stays valid when the scrape interval is short. Three
	// minutes covers several missed AWS scrapes at any supported interval and matches the request
	// window, so a recovering instance is never expired while its events are still reachable.
	minMetricsTTL = 3 * time.Minute

	// staleRetention keeps an expired instance reported as down long enough for an alert to fire
	// before its series disappears.
	staleRetention = 15 * time.Minute

	ttlIntervals = 3

	upMetricName           = "rds_exporter_enhanced_up"
	lastEventMetricName    = "rds_exporter_enhanced_last_event_timestamp_seconds"
	scrapeErrorsMetricName = "rds_exporter_enhanced_scrape_errors_total"

	regionLabel   = "region"
	instanceLabel = "instance"
	kindLabel     = "kind"

	// goroutinesPerSession is the scraper plus the goroutine draining its results.
	goroutinesPerSession = 2
)

// instanceState is the latest sample of a single instance, and how long it stays valid.
type instanceState struct {
	metrics   []prometheus.Metric
	eventTime time.Time
	expiresAt time.Time
	// receivedAt is when the sample was stored. Retention is measured against it rather than against
	// the event timestamp, which CloudWatch lets the monitored account choose.
	receivedAt time.Time
}

type errorKey struct {
	region string
	kind   string
}

// Collector collects enhanced RDS metrics by utilizing several scrapers.
type Collector struct {
	logger log.Logger

	upDesc        *prometheus.Desc
	lastEventDesc *prometheus.Desc
	errorsDesc    *prometheus.Desc

	cancel context.CancelFunc
	wg     sync.WaitGroup

	// configured is every instance the collector monitors, so health can be reported for instances
	// that have never delivered a sample.
	configured map[instanceKey]struct{}

	rw      sync.RWMutex
	metrics map[instanceKey]instanceState
	errors  map[errorKey]uint64
}

// metricsTTL returns how long a sample stays valid for the given scrape interval.
func metricsTTL(interval time.Duration) time.Duration {
	return max(ttlIntervals*interval, minMetricsTTL)
}

func newCollector(logger log.Logger) *Collector {
	return &Collector{
		logger: log.With(logger, "component", "enhanced"),
		upDesc: prometheus.NewDesc(upMetricName,
			"Whether Enhanced Monitoring metrics for this instance are current (1) or stale (0).",
			[]string{regionLabel, instanceLabel}, nil),
		lastEventDesc: prometheus.NewDesc(lastEventMetricName,
			"Timestamp of the most recent Enhanced Monitoring event received for this instance.",
			[]string{regionLabel, instanceLabel}, nil),
		errorsDesc: prometheus.NewDesc(scrapeErrorsMetricName,
			"Number of failed Enhanced Monitoring scrapes, by error kind.",
			[]string{regionLabel, kindLabel}, nil),
		cancel:     nil,
		wg:         sync.WaitGroup{},
		configured: make(map[instanceKey]struct{}),
		rw:         sync.RWMutex{},
		metrics:    make(map[instanceKey]instanceState),
		errors:     make(map[errorKey]uint64),
	}
}

// NewCollector creates new collector and starts scrapers.
func NewCollector(sessions *sessions.Sessions, logger log.Logger) *Collector {
	collector := newCollector(logger)

	ctx, cancel := context.WithCancel(context.Background())
	collector.cancel = cancel

	for session, enabledInstances := range collector.configure(sessions.AllSessions()) {
		cfg := sessions.Configs[session]
		s := newScraper(cfg, enabledInstances, logger)

		level.Info(s.logger).Log("msg", fmt.Sprintf("Updating enhanced metrics every %s.", s.interval()))

		// perform first scrapes synchronously so returned collector has all metric descriptions
		metrics, _ := s.scrape(ctx)
		collector.setMetrics(s.result(metrics), time.Now())

		results := make(chan scrapeResult)

		collector.wg.Add(goroutinesPerSession)

		go func() {
			defer collector.wg.Done()

			for result := range results {
				collector.setMetrics(result, time.Now())
			}
		}()

		go func() {
			defer collector.wg.Done()

			s.start(ctx, results)
		}()
	}

	return collector
}

// configure records every instance the collector monitors and returns them grouped by session. The
// set has to be complete before the first scraper starts, because prune reads it from every drain
// goroutine and nothing writes it afterwards.
func (c *Collector) configure(all map[string][]sessions.Instance) map[string][]sessions.Instance {
	enabled := make(map[string][]sessions.Instance, len(all))

	for session, instances := range all {
		enabled[session] = getEnabledInstances(instances)
		for _, instance := range enabled[session] {
			c.configured[keyOf(instance)] = struct{}{}
		}
	}

	return enabled
}

// Stop stops the scrapers and waits for them to finish.
func (c *Collector) Stop() {
	if c.cancel == nil {
		return
	}

	c.cancel()
	c.wg.Wait()
}

func getEnabledInstances(instances []sessions.Instance) []sessions.Instance {
	enabledInstances := make([]sessions.Instance, 0, len(instances))

	for _, instance := range instances {
		if instance.DisableEnhancedMetrics {
			continue
		}

		enabledInstances = append(enabledInstances, instance)
	}

	return enabledInstances
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(_ chan<- *prometheus.Desc) {
	// unchecked collector
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(out chan<- prometheus.Metric) {
	c.rw.RLock()
	defer c.rw.RUnlock()

	c.collectSamples(out, time.Now())
	c.collectSilentInstances(out)
	c.collectErrors(out)
}

// collectSamples emits the metrics of the instances that reported, and the health of all of them.
// An expired sample contributes no metrics, so an outage renders as a gap rather than a flat line.
func (c *Collector) collectSamples(out chan<- prometheus.Metric, now time.Time) {
	for key, state := range c.metrics {
		current := now.Before(state.expiresAt)
		if current {
			for _, m := range state.metrics {
				out <- m
			}
		}

		out <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, boolToFloat(current),
			key.region, key.instance)

		if !state.eventTime.IsZero() {
			out <- prometheus.MustNewConstMetric(c.lastEventDesc, prometheus.GaugeValue,
				float64(state.eventTime.Unix()), key.region, key.instance)
		}
	}
}

// collectSilentInstances reports the instances that have never delivered a sample as down. Enhanced
// Monitoring disabled in AWS, or a log stream that does not exist, otherwise leaves an instance with
// no series at all, which can only be alerted on with absent().
func (c *Collector) collectSilentInstances(out chan<- prometheus.Metric) {
	for key := range c.configured {
		if _, reported := c.metrics[key]; reported {
			continue
		}

		out <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, 0, key.region, key.instance)
	}
}

func (c *Collector) collectErrors(out chan<- prometheus.Metric) {
	for key, count := range c.errors {
		out <- prometheus.MustNewConstMetric(c.errorsDesc, prometheus.CounterValue, float64(count),
			key.region, key.kind)
	}
}

// setMetrics saves the latest scraped metrics and drops instances that stopped reporting long ago.
func (c *Collector) setMetrics(result scrapeResult, now time.Time) {
	c.rw.Lock()
	defer c.rw.Unlock()

	ttl := metricsTTL(result.interval)

	for key, fresh := range result.metrics {
		previous := c.metrics[key]

		// The request window starts at the newest event of the slowest instance, so that event is
		// re-delivered on every scrape. Expiry follows the event timestamp to keep an outage visible.
		// A released payload has nothing left to protect, and refusing the sample on its timestamp
		// alone would strand an instance whose replacement publishes older ones.
		if previous.metrics != nil && !fresh.eventTime.After(previous.eventTime) {
			continue
		}

		c.metrics[key] = instanceState{
			metrics:    fresh.metrics,
			eventTime:  fresh.eventTime,
			expiresAt:  fresh.eventTime.Add(ttl),
			receivedAt: now,
		}
	}

	for kind, count := range result.errorCounts {
		c.errors[errorKey{region: result.region, kind: kind}] += count
	}

	c.prune(now)
}

// prune releases what has not been refreshed for staleRetention. A monitored instance keeps its
// entry without its payload, so a long outage stays reported as down instead of resolving the alert
// by making the series disappear; anything else, such as a retired resource ID, is dropped.
func (c *Collector) prune(now time.Time) {
	for key, state := range c.metrics {
		if now.Sub(state.receivedAt) <= staleRetention {
			continue
		}

		if _, configured := c.configured[key]; !configured {
			delete(c.metrics, key)

			continue
		}

		state.metrics = nil
		c.metrics[key] = state
	}
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}

	return 0
}

// check interfaces
var (
	_ prometheus.Collector = (*Collector)(nil)
)
