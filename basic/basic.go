package basic

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"

	"github.com/hellofresh/rds_exporter/config"
	"github.com/hellofresh/rds_exporter/sessions"
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

// Metric is an entity used to describe a metric value
type Metric struct {
	Name string
	Desc *prometheus.Desc
}

// Exporter is an entity used to describe an exporter
type Exporter struct {
	config   *config.Config
	sessions *sessions.Sessions
	metrics  []Metric
	l        log.Logger
}

// New creates a new instance of a Exporter.
func New(config *config.Config, sessions *sessions.Sessions) *Exporter {
	return &Exporter{
		config:   config,
		sessions: sessions,
		metrics:  Metrics,
		l:        log.With("component", "basic"),
	}
}

// Collect is a function used to collect metrics
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	e.collect(ch)

	// Collect scrape time
	ch <- prometheus.MustNewConstMetric(scrapeTimeDesc, prometheus.GaugeValue, time.Since(now).Seconds())
}

func (e *Exporter) collect(ch chan<- prometheus.Metric) {
	wg := &sync.WaitGroup{}
	defer wg.Wait()

	instances := e.config.Instances
	wg.Add(len(instances))
	for _, instance := range instances {
		instance := instance
		go func() {
			NewScraper(&instance, e, ch).Scrape()
			wg.Done()
		}()
	}
}

// Describe is a function used to describe metrics
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	// RDS metrics
	for _, m := range e.metrics {
		ch <- m.Desc
	}

	// Scrape time
	ch <- scrapeTimeDesc
}
