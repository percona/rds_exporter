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

const (
	testRegion  = "us-east-2"
	testAurora  = "a3"
	testMySQL   = "m84"
	labelEngine = "aws_engine"
	labelNode   = "node_name"
)

func TestCollector(t *testing.T) {
	t.Parallel()

	src := fakeSource{
		testRegion: {
			{ //nolint:exhaustruct
				Region:                 testRegion,
				Instance:               testAurora,
				Engine:                 "aurora-mysql",
				ClusterID:              "cluster-a",
				AccountID:              "123456789012",
				Labels:                 map[string]string{labelNode: testAurora, labelEngine: "spoof"},
				DisableBasicMetrics:    true,
				DisableEnhancedMetrics: true,
			},
			{ //nolint:exhaustruct
				Region:   testRegion,
				Instance: testMySQL,
				Engine:   "mysql",
				Labels:   map[string]string{labelNode: testMySQL},
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

		got[labels[labelInstance]] = labels
	}

	assert.Len(t, families[0].GetMetric(), 2)
	assert.Equal(t, map[string]map[string]string{
		testAurora: {
			labelRegion: testRegion, labelInstance: testAurora, labelNode: testAurora,
			labelEngine: "aurora-mysql", "aws_cluster_id": "cluster-a", "aws_account_id": "123456789012",
		},
		testMySQL: {
			labelRegion: testRegion, labelInstance: testMySQL, labelNode: testMySQL,
			labelEngine: "mysql",
		},
	}, got)
}
