package basic

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"

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
func New(config *config.Config, sessions *sessions.Sessions) *Collector {
	return &Collector{
		config:   config,
		sessions: sessions,
		metrics:  Metrics,
		l:        log.With("component", "basic"),
	}
}

func (e *Collector) Describe(ch chan<- *prometheus.Desc) {
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

	wg.Add(len(e.config.Instances))
	for _, instance := range e.config.Instances {
		instance := instance
		go func() {
			defer wg.Done()

			s := NewScraper(&instance, e, ch)
			if s == nil {
				e.l.Errorf("No scraper for %s, skipping.", instance)
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
