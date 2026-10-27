package enhanced

import (
	"sort"
	"testing"

	"github.com/percona/exporter_shared/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	for _, data := range []struct {
		region   string
		instance string
	}{
		{"us-east-2", "pmm-qa-aurora3-mysql-instance-1"},
		{"us-east-2", "pmm-qa-aurora-postgres-13-5-instance-1"},
		{"us-east-2", "pmm-qa-rds-mysql-8-4"},
		{"us-east-2", "pmm-qa-pgsql-16"},
	} {
		t.Run(data.instance, func(t *testing.T) {
			// Test that metrics created from fixed testdata JSON produce expected result.

			m, err := parseOSMetrics(readTestDataJSON(t, data.instance), true)
			require.NoError(t, err)

			actualMetrics := helpers.ReadMetrics(m.makePrometheusMetrics(data.region, nil))
			sort.Slice(actualMetrics, func(i, j int) bool { return actualMetrics[i].Less(actualMetrics[j]) })
			actualLines := helpers.Format(helpers.WriteMetrics(actualMetrics))

			if *goldenTXT {
				writeTestDataMetrics(t, data.instance, actualLines)
			}

			expectedLines := readTestDataMetrics(t, data.instance)
			expectedMetrics := helpers.ReadMetrics(helpers.Parse(expectedLines))
			sort.Slice(expectedMetrics, func(i, j int) bool { return expectedMetrics[i].Less(expectedMetrics[j]) })

			// compare both to try to avoid go-difflib bug
			assert.Equal(t, expectedLines, actualLines)
			assert.Equal(t, expectedMetrics, actualMetrics)
		})
	}
}

func TestParseUptime(t *testing.T) {
	t.Skip("TODO Parse uptime https://jira.percona.com/browse/PMM-2131")

	_ = "01:45:58"
	_ = "1 day, 07:11:58"
}
