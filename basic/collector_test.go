package basic

import (
	"testing"

	"github.com/percona/exporter_shared/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/percona/rds_exporter/client"
	"github.com/percona/rds_exporter/config"
	"github.com/percona/rds_exporter/sessions"
)

func TestCollector(t *testing.T) {
	cfg, err := config.Load("../config.tests.yml")
	require.NoError(t, err)
	client := client.New()
	sess, err := sessions.New(cfg.Instances, client.HTTP(), false)
	require.NoError(t, err)

	c := New(cfg, sess)

	metrics := helpers.ReadMetrics(helpers.CollectMetrics(c))
	for _, m := range metrics {
		m.Value = 0
	}
	actual := helpers.Format(helpers.WriteMetrics(metrics))

	if *goldenTXT {
		writeTestDataMetrics(t, actual)
	}

	metrics = helpers.ReadMetrics(helpers.Parse(readTestDataMetrics(t)))
	for _, m := range metrics {
		m.Value = 0
	}
	expected := helpers.Format(helpers.WriteMetrics(metrics))

	assert.Equal(t, expected, actual)
}
