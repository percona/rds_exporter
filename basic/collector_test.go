package basic

import (
	"sort"
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

	actualMetrics := helpers.ReadMetrics(helpers.CollectMetrics(c))
	sort.Slice(actualMetrics, func(i, j int) bool { return actualMetrics[i].Less(actualMetrics[j]) })
	actualLines := helpers.Format(helpers.WriteMetrics(actualMetrics))

	if *goldenTXT {
		writeTestDataMetrics(t, actualLines)
	}

	for _, m := range actualMetrics {
		m.Value = 0
	}
	actualLines = helpers.Format(helpers.WriteMetrics(actualMetrics))

	expectedMetrics := helpers.ReadMetrics(helpers.Parse(readTestDataMetrics(t)))
	sort.Slice(expectedMetrics, func(i, j int) bool { return expectedMetrics[i].Less(expectedMetrics[j]) })
	for _, m := range expectedMetrics {
		m.Value = 0
	}
	expectedLines := helpers.Format(helpers.WriteMetrics(expectedMetrics))

	// compare both to try to avoid go-difflib bug
	assert.Equal(t, expectedLines, actualLines)
	assert.Equal(t, expectedMetrics, actualMetrics)
}
