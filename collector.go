package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/Percona-Lab/rds_exporter/config"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	templates = map[string]*cwCollectorTemplate{}
)

type cwMetric struct {
	Desc    *prometheus.Desc
	ValType prometheus.ValueType

	ConfMetric  *config.Metric
	LabelNames  []string
	LabelValues []string
}

type cwCollectorTemplate struct {
	Metrics []cwMetric
	Task    *config.Task
}

type cwCollector struct {
	Region            string
	Target            string
	ScrapeTime        prometheus.Gauge
	ErroneousRequests prometheus.Counter
	Template          *cwCollectorTemplate
}

// generateTemplates creates pre-generated metrics descriptions so that only the metrics are created from them during a scrape.
func generateTemplates(cfg *config.Settings) {
	for t := range cfg.Tasks {
		var template = new(cwCollectorTemplate)

		//Save the task it belongs to
		template.Task = &cfg.Tasks[t]

		//Pre-allocate at least a few metrics
		template.Metrics = make([]cwMetric, 0, len(cfg.Tasks[t].Metrics))

		for m := range cfg.Tasks[t].Metrics {
			metric := &cfg.Tasks[t].Metrics[m]

			labels := make([]string, len(metric.Dimensions))

			for i := range metric.Dimensions {
				labels[i] = toSnakeCase(metric.Dimensions[i])
			}
			labels = append(labels, "task")

			for s := range metric.Statistics {
				template.Metrics = append(template.Metrics, cwMetric{
					Desc: prometheus.NewDesc(
						safeName(toSnakeCase(metric.Namespace)+"_"+toSnakeCase(metric.Name)+"_"+toSnakeCase(metric.Statistics[s])),
						metric.Name,
						labels,
						nil),
					ValType:    prometheus.GaugeValue,
					ConfMetric: metric,
					LabelNames: labels,
				})
			}
		}

		templates[cfg.Tasks[t].Name] = template
	}
}

// NewCwCollector creates a new instance of a CwCollector for a specific task
// The newly created instance will reference its parent template so that metric descriptions are not recreated on every call.
// It returns either a pointer to a new instance of cwCollector or an error.
func NewCwCollector(target string, taskName string, region string) (*cwCollector, error) {
	if settings == nil {
		return nil, fmt.Errorf("settings not initialized")
	}

	// Check if task exists
	task, err := settings.GetTask(taskName)

	if err != nil {
		return nil, err
	}

	if region == "" {
		if task.DefaultRegion == "" {
			return nil, errors.New("No region or default region set requested task")
		} else {
			region = task.DefaultRegion
		}
	}

	return &cwCollector{
		Region: region,
		Target: target,
		ScrapeTime: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cloudwatch_exporter_scrape_duration_seconds",
			Help: "Time this CloudWatch scrape took, in seconds.",
		}),
		ErroneousRequests: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cloudwatch_exporter_erroneous_requests",
			Help: "The number of erroneous request made by this scrape.",
		}),
		Template: templates[taskName],
	}, nil
}

func (c *cwCollector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	scrape(c, ch)
	c.ScrapeTime.Set(time.Since(now).Seconds())

	ch <- c.ScrapeTime
	ch <- c.ErroneousRequests
}

func (c *cwCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.ScrapeTime.Desc()
	ch <- c.ErroneousRequests.Desc()

	for m := range c.Template.Metrics {
		ch <- c.Template.Metrics[m].Desc
	}
}
