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

// Collector collects enhanced RDS metrics by utilizing several scrapers.
type Collector struct {
	sessions *sessions.Sessions
	logger   log.Logger

	rw      sync.RWMutex
	metrics map[string][]prometheus.Metric
}

// Maximal and minimal metrics update interval.
const (
	defaultInterval = 60 * time.Second
	maxInterval     = 1800 * time.Second
	minInterval     = 2 * time.Second
)

// NewCollector creates new collector and starts scrapers.
func NewCollector(sessions *sessions.Sessions, logger log.Logger) *Collector {
	c := &Collector{
		sessions: sessions,
		logger:   log.With(logger, "component", "enhanced"),
		metrics:  make(map[string][]prometheus.Metric),
	}

	for session, instances := range sessions.AllSessions() {
		enabledInstances := getEnabledInstances(instances)
		cfg := sessions.Configs[session]
		s := newScraper(cfg, enabledInstances, logger)

		interval := defaultInterval
		for _, instance := range enabledInstances {
			interval = getMonitoringInterval(instance.MonitoringInterval, s.logger)
			level.Info(s.logger).Log("msg",
				fmt.Sprintf("Updating enhanced metrics every %v, for instance: %s", interval, instance.Instance))
		}

		// perform first scrapes synchronously so returned collector has all metric descriptions
		m, _ := s.scrape(context.TODO())
		c.setMetrics(m)

		ch := make(chan map[string][]prometheus.Metric)
		go func() {
			for m := range ch {
				c.setMetrics(m)
			}
		}()
		go s.start(context.TODO(), interval, ch)
	}

	return c
}

func getMonitoringInterval(interval time.Duration, logger log.Logger) time.Duration {
	if int(interval) > 0 {
		if int(interval) < int(maxInterval) && int(interval) > int(minInterval) {
			return interval
		} else if int(interval) < int(minInterval) {
			level.Warn(logger).Log("msg",
				fmt.Sprintf("Interval '%d' is less than the minimum interval,"+
					"setting the interval to the minimum value: %ds", interval/time.Second, minInterval/time.Second))
			return minInterval
		} else if int(interval) > int(maxInterval) {
			level.Warn(logger).Log("msg",
				fmt.Sprintf("Interval %d equals or exceeeds the maximum interval,"+
					"setting the interval to the maximum value: %ds", interval/time.Second, maxInterval/time.Second))
			return maxInterval
		}
	}

	return defaultInterval
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

// setMetrics saves latest scraped metrics.
func (c *Collector) setMetrics(m map[string][]prometheus.Metric) {
	c.rw.Lock()
	for id, metrics := range m {
		c.metrics[id] = metrics
	}
	c.rw.Unlock()
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(chan<- *prometheus.Desc) {
	// unchecked collector
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.rw.RLock()
	defer c.rw.RUnlock()

	for _, metrics := range c.metrics {
		for _, m := range metrics {
			ch <- m
		}
	}
}

// check interfaces
var (
	_ prometheus.Collector = (*Collector)(nil)
)
