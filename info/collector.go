package info

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/percona/rds_exporter/sessions"
)

// InstanceSource provides the instances to publish info series for.
type InstanceSource interface {
	AllSessions() map[string][]sessions.Instance
}

// Collector publishes one aws_rds_instance_info series per instance.
type Collector struct {
	src InstanceSource
}

// NewCollector creates a Collector reading instances from src.
func NewCollector(src InstanceSource) *Collector {
	return &Collector{src: src}
}

// Describe implements prometheus.Collector; it sends nothing, making this an unchecked collector.
func (c *Collector) Describe(chan<- *prometheus.Desc) {}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	for _, instances := range c.src.AllSessions() {
		for _, instance := range instances {
			labels := prometheus.Labels{
				"region":   instance.Region,
				"instance": instance.Instance,
			}
			for n, v := range instance.Labels {
				labels[n] = v
			}
			// Applied last so a config label cannot overwrite the AWS value.
			labels["aws_cluster_id"] = instance.ClusterID
			labels["aws_engine"] = instance.Engine
			labels["aws_account_id"] = instance.AccountID

			desc := prometheus.NewDesc("aws_rds_instance_info", "Information about the RDS instance.", nil, labels)
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 1)
		}
	}
}
