package enhanced

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"

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
	maxInterval = 10 * time.Second
	minInterval = 2 * time.Second
)

// NewCollector creates new collector and starts scrapers.
func NewCollector(sessions *sessions.Sessions) *Collector {
	c := &Collector{
		sessions: sessions,
		logger:   log.With("component", "enhanced"),
		metrics:  make(map[string][]prometheus.Metric),
	}

	for session, instances := range sessions.AllSessions() {
		s := newScraper(session, instances)

		interval := maxInterval
		for _, instance := range instances {
			if instance.EnhancedMonitoringInterval > 0 && instance.EnhancedMonitoringInterval < interval {
				interval = instance.EnhancedMonitoringInterval
			}
		}
		if interval < minInterval {
			interval = minInterval
		}
		s.logger.Infof("Updating enhanced metrics every %s.", interval)

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

// setMetrics saves latest scraped metrics.
func (c *Collector) setMetrics(m map[string][]prometheus.Metric) {
	c.rw.Lock()
	for id, metrics := range m {
		c.metrics[id] = metrics
	}
	c.rw.Unlock()
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
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
