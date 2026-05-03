package basic

import (
	"fmt"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/percona/rds_exporter/config"
	"github.com/percona/rds_exporter/sessions"
)

//go:generate go run generate/main.go generate/utils.go

var (
	scrapeTimeDesc = prometheus.NewDesc(
		"rds_exporter_scrape_duration_seconds",
		"Time this RDS scrape took, in seconds.",
		[]string{},
		nil,
	)
)

type Metric struct {
	cwName         string
	prometheusName string
	prometheusHelp string
}

type Collector struct {
	config   *config.Config
	sessions *sessions.Sessions
	metrics  []Metric
	l        log.Logger
}

// New creates a new instance of a Collector.
func New(config *config.Config, sessions *sessions.Sessions, logger log.Logger) *Collector {
	return &Collector{
		config:   config,
		sessions: sessions,
		metrics:  Metrics,
		l:        log.With(logger, "component", "basic"),
	}
}

func (e *Collector) Describe(chan<- *prometheus.Desc) {
	// unchecked collector
}

func (e *Collector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	e.collect(ch)

	// Collect scrape time
	ch <- prometheus.MustNewConstMetric(scrapeTimeDesc, prometheus.GaugeValue, time.Since(now).Seconds())
}

func (e *Collector) collect(ch chan<- prometheus.Metric) {
	var wg sync.WaitGroup
	defer wg.Wait()

	for _, instance := range e.config.Instances {
		if instance.DisableBasicMetrics {
			level.Debug(e.l).Log("msg", fmt.Sprintf("Instance %s has disabled basic metrics, skipping.", instance))
			continue
		}
		instance := instance
		wg.Add(1)
		go func() {
			defer wg.Done()

			s := NewScraper(&instance, e, ch)
			if s == nil {
				level.Error(e.l).Log("msg", fmt.Sprintf("No scraper for %s, skipping.", instance))
				return
			}
			s.Scrape()
		}()
	}
}

// check interfaces
var (
	_ prometheus.Collector = (*Collector)(nil)
)
