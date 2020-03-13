package basic

import (
	"fmt"
	"sort"
	"strings"
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

func TestCollectorDisableBasicMetrics(t *testing.T) {
	cfg, err := config.Load("../config.tests.yml")
	require.NoError(t, err)
	client := client.New()
	instanceGroups := make(map[bool][]string, 2)
	for i := range cfg.Instances {
		// Disable basic metrics in even instances.
		// This disable instance: no-such-instance.
		isDisabled := i%2 == 0
		cfg.Instances[i].DisableBasicMetrics = isDisabled
		// Groups instance names by disabled or enabled metrics.
		instanceGroups[isDisabled] = append(instanceGroups[isDisabled], cfg.Instances[i].Instance)
	}
	sess, err := sessions.New(cfg.Instances, client.HTTP(), false)
	require.NoError(t, err)

	c := New(cfg, sess)

	actualMetrics := helpers.ReadMetrics(helpers.CollectMetrics(c))
	actualLines := helpers.Format(helpers.WriteMetrics(actualMetrics))

	// Check if all collected metrics do not contain metrics for instance whare disabled metrics.
	hasMetricForInstance := func(lines []string, instanceName string) bool {
		for _, line := range lines {
			if strings.Contains(line, fmt.Sprintf("instance=%q", instanceName)) {
				return true
			}
		}
		return false
	}

	// Scans if metrics contain a metric for the disabled instance (DisableBasicMetrics = true).
	for _, inst := range instanceGroups[true] {
		assert.Falsef(t, hasMetricForInstance(actualLines, inst), "Found metrics for disabled instance %s", inst)
	}

	// Scans if metrics contain a metric for the enabled instance (DisableBasicMetrics = false).
	for _, inst := range instanceGroups[false] {
		assert.Truef(t, hasMetricForInstance(actualLines, inst), "Did not find metrics for enabled instance %s", inst)
	}
}
