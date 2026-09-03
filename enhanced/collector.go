package enhanced

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/percona/rds_exporter/sessions"
)

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
	clockSkewMetricName    = "rds_exporter_enhanced_clock_skew_events_total"

	regionLabel   = "region"
	instanceLabel = "instance"
	kindLabel     = "kind"
)

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
	skewDesc      *prometheus.Desc

	cancel context.CancelFunc
	wg     sync.WaitGroup

	// configured is every instance the collector monitors, so health can be reported for instances
	// that have never delivered a sample.
	configured map[instanceKey]struct{}

	rw sync.RWMutex
	// monitored is whether AWS has Enhanced Monitoring on for a configured instance, as of its
	// session's last scrape. It is guarded because Enhanced Monitoring can be turned on or off while
	// the exporter runs, unlike the configured set.
	monitored map[instanceKey]bool
	metrics   map[instanceKey]instanceState
	errors    map[errorKey]uint64
	// skewed is how many events a region delivered timestamped ahead of the exporter's own clock.
	skewed map[string]uint64
}

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
			"Enhanced Monitoring collection errors by kind; not_found counts log streams newly excluded, "+
				"group_not_found the log group of a whole session.",
			[]string{regionLabel, kindLabel}, nil),
		skewDesc: prometheus.NewDesc(clockSkewMetricName,
			"Enhanced Monitoring events timestamped ahead of the exporter's clock; their samples are exported regardless.",
			[]string{regionLabel}, nil),
		cancel:     nil,
		wg:         sync.WaitGroup{},
		configured: make(map[instanceKey]struct{}),
		rw:         sync.RWMutex{},
		monitored:  make(map[instanceKey]bool),
		metrics:    make(map[instanceKey]instanceState),
		errors:     make(map[errorKey]uint64),
		skewed:     make(map[string]uint64),
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
		metrics := s.scrapeOnce(ctx, s.interval())
		collector.setMetrics(s.result(metrics), time.Now())

		results := make(chan scrapeResult)

		collector.wg.Go(func() {
			for result := range results {
				collector.setMetrics(result, time.Now())
			}
		})

		collector.wg.Go(func() {
			s.start(ctx, results)
		})
	}

	return collector
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
	c.collectClockSkew(out)
}

// configure records every instance the collector monitors and returns them grouped by session. The
// set has to be complete before the first scraper starts, because prune reads it from every drain
// goroutine and nothing writes it afterwards.
func (c *Collector) configure(all map[string][]sessions.Instance) map[string][]sessions.Instance {
	enabled := make(map[string][]sessions.Instance, len(all))

	for session, instances := range all {
		enabledInstances := getEnabledInstances(instances)

		// A session left without instances has no log stream to request and no region to report
		// under, so a scraper for it would poll AWS for nothing and publish its self-metrics with an
		// empty region label. The flag it was filtered on comes from the configuration file, which
		// cannot change while the exporter runs, so nothing can arrive later to justify one.
		if len(enabledInstances) == 0 {
			continue
		}

		enabled[session] = enabledInstances
		for _, instance := range enabledInstances {
			c.configured[keyOf(instance)] = struct{}{}
		}
	}

	return enabled
}

// collectSamples emits the stored sample of every instance along with its health. An expired sample
// contributes no metrics, so an outage renders as a gap rather than a flat line.
func (c *Collector) collectSamples(out chan<- prometheus.Metric, now time.Time) {
	for key, state := range c.metrics {
		current := now.Before(state.expiresAt)
		if current {
			for _, m := range state.metrics {
				out <- m
			}
		}

		if current || !c.silenced(key) {
			out <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, boolToFloat(current),
				key.region, key.instance)
		}

		if !state.eventTime.IsZero() {
			out <- prometheus.MustNewConstMetric(c.lastEventDesc, prometheus.GaugeValue,
				float64(state.eventTime.Unix()), key.region, key.instance)
		}
	}
}

// collectSilentInstances reports the instances that have never delivered a sample as down. A log
// stream that does not exist otherwise leaves an instance with no series at all, which can only be
// alerted on with absent().
func (c *Collector) collectSilentInstances(out chan<- prometheus.Metric) {
	for key := range c.configured {
		if _, reported := c.metrics[key]; reported {
			continue
		}

		if c.silenced(key) {
			continue
		}

		out <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, 0, key.region, key.instance)
	}
}

// silenced reports whether AWS has Enhanced Monitoring off for the instance, which leaves it nothing
// to publish and no log stream for the scraper to request. Calling that down would be a zero no
// operator could clear, and prune keeps the entry alive so it cannot even age out. An instance no
// scrape has reported on yet is not silenced, so a key nothing accounts for is still reported down.
func (c *Collector) silenced(key instanceKey) bool {
	monitored, known := c.monitored[key]

	return known && !monitored
}

// collectClockSkew reports how many events a region delivered dated ahead of the exporter's clock.
// It is deliberately not one of the error kinds: the samples are exported either way, so counting it
// there would make a host whose clock drifts look like collection failing.
func (c *Collector) collectClockSkew(out chan<- prometheus.Metric) {
	for region, count := range c.skewed {
		out <- prometheus.MustNewConstMetric(c.skewDesc, prometheus.CounterValue, float64(count), region)
	}
}

func (c *Collector) collectErrors(out chan<- prometheus.Metric) {
	for key, count := range c.errors {
		out <- prometheus.MustNewConstMetric(c.errorsDesc, prometheus.CounterValue, float64(count),
			key.region, key.kind)
	}
}

func (c *Collector) setMetrics(result scrapeResult, now time.Time) {
	c.rw.Lock()
	defer c.rw.Unlock()

	ttl := metricsTTL(result.interval)

	maps.Copy(c.monitored, result.monitored)

	for key, fresh := range result.metrics {
		previous := c.metrics[key]

		// The request window starts at the newest event of the slowest instance, so that event is
		// re-delivered on every scrape and must not keep extending the sample's life. Only the
		// redelivery is refused, not every older timestamp: an event CloudWatch let the monitored
		// account date in the future would otherwise sit in front of every real event until the
		// clock caught up, and the instance would report itself down meanwhile. A released payload
		// has nothing left to protect, so a replacement instance is taken whatever it publishes.
		if previous.metrics != nil && fresh.eventTime.Equal(previous.eventTime) {
			continue
		}

		c.metrics[key] = instanceState{
			metrics:    fresh.metrics,
			eventTime:  fresh.eventTime,
			expiresAt:  notAfter(fresh.eventTime, now).Add(ttl),
			receivedAt: now,
		}
	}

	for kind, count := range result.errorCounts {
		c.errors[errorKey{region: result.region, kind: kind}] += count
	}

	// Assigned even when nothing was skewed, so the counter starts from a zero the region publishes
	// rather than appearing only once a clock has already drifted.
	c.skewed[result.region] += result.skewedEvents

	c.prune(now)
}

// prune releases the payload of what has not been refreshed for staleRetention. A monitored instance
// keeps its entry, so a long outage stays reported as down instead of resolving the alert by making
// the series disappear; an entry for an instance the collector does not monitor is dropped, so a key
// that should not be there cannot report health forever.
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

// notAfter caps a time at a limit. Expiry runs from whichever came first, the event or its receipt,
// so a timestamp CloudWatch let the monitored account choose cannot keep a sample current for hours
// after the instance stopped reporting.
func notAfter(t, limit time.Time) time.Time {
	if t.After(limit) {
		return limit
	}

	return t
}

// notBefore floors a time at a limit, the mirror of notAfter.
func notBefore(t, limit time.Time) time.Time {
	if t.Before(limit) {
		return limit
	}

	return t
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
