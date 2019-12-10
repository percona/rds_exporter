package basic

import (
	"testing"

	"github.com/percona/exporter_shared/helpers"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/percona/rds_exporter/client"
	"github.com/percona/rds_exporter/config"
	"github.com/percona/rds_exporter/sessions"
)

func getCollector(t *testing.T) *Collector {
	t.Helper()

	cfg, err := config.Load("../config.tests.yml")
	require.NoError(t, err)
	client := client.New()
	sess, err := sessions.New(cfg.Instances, client.HTTP(), false)
	require.NoError(t, err)
	return New(cfg, sess)
}

func TestCollector_Describe(t *testing.T) {
	c := getCollector(t)
	ch := make(chan *prometheus.Desc)
	go func() {
		c.Describe(ch)
		close(ch)
	}()

	const expected = 50
	descs := make([]*prometheus.Desc, 0, expected)
	for d := range ch {
		descs = append(descs, d)
	}

	assert.Equal(t, expected, len(descs), "%+v", descs)
}

func TestCollector_Collect(t *testing.T) {
	c := getCollector(t)
	ch := make(chan prometheus.Metric)
	go func() {
		c.Collect(ch)
		close(ch)
	}()

	const expected = 101
	metrics := make([]helpers.Metric, 0, expected)
	for m := range ch {
		metrics = append(metrics, *helpers.ReadMetric(m))
	}

	assert.Equal(t, expected, len(metrics), "%+v", metrics)
}
