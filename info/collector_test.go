package info

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/percona/rds_exporter/sessions"
)

type fakeSource map[string][]sessions.Instance

func (f fakeSource) AllSessions() map[string][]sessions.Instance { return f }

func TestCollector(t *testing.T) {
	src := fakeSource{
		"us-east-2": {
			{
				Region:                 "us-east-2",
				Instance:               "a3",
				Engine:                 "aurora-mysql",
				ClusterID:              "cluster-a",
				AccountID:              "123456789012",
				Labels:                 map[string]string{"node_name": "a3", "aws_engine": "spoof"},
				DisableBasicMetrics:    true,
				DisableEnhancedMetrics: true,
			},
			{
				Region:   "us-east-2",
				Instance: "m84",
				Engine:   "mysql",
				Labels:   map[string]string{"node_name": "m84"},
			},
		},
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(NewCollector(src))
	families, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)
	assert.Equal(t, "aws_rds_instance_info", families[0].GetName())

	got := map[string]map[string]string{}
	for _, m := range families[0].GetMetric() {
		assert.InDelta(t, 1, m.GetGauge().GetValue(), 0)
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			if l.GetValue() != "" {
				labels[l.GetName()] = l.GetValue()
			}
		}
		got[labels["instance"]] = labels
	}

	assert.Len(t, families[0].GetMetric(), 2)
	assert.Equal(t, map[string]map[string]string{
		"a3": {
			"region": "us-east-2", "instance": "a3", "node_name": "a3",
			"aws_engine": "aurora-mysql", "aws_cluster_id": "cluster-a", "aws_account_id": "123456789012",
		},
		"m84": {
			"region": "us-east-2", "instance": "m84", "node_name": "m84",
			"aws_engine": "mysql",
		},
	}, got)
}
