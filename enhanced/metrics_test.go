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
		{"us-east-1", "aurora-mysql-56"},
		{"us-west-1", "psql-10"},
		{"us-west-2", "mysql-57"},
		{"us-west-2", "aurora-psql-11"},
	} {
		data := data
		t.Run(data.instance, func(t *testing.T) {
			// Test that metrics created from fixed testdata JSON produce expected result.

			d := readTestDataJSON(t, data.instance)

			m, err := parseOSMetrics(d, true)
			require.NoError(t, err)

			m2, err := parseOSMetrics(d, true)
			require.NoError(t, err)

			actualMetrics := helpers.ReadMetrics(m.makePrometheusMetrics(data.region, nil))
			sort.Slice(actualMetrics, func(i, j int) bool { return actualMetrics[i].Less(actualMetrics[j]) })
			actualLines := helpers.Format(helpers.WriteMetrics(actualMetrics))

			actualMetrics2 := helpers.ReadMetrics(m2.makePrometheusMetrics(data.region, nil))
			sort.Slice(actualMetrics2, func(i, j int) bool { return actualMetrics2[i].Less(actualMetrics2[j]) })

			if *goldenTXT {
				writeTestDataMetrics(t, data.instance, actualLines)
			}

			expectedLines := readTestDataMetrics(t, data.instance)
			expectedMetrics := helpers.ReadMetrics(helpers.Parse(expectedLines))
			sort.Slice(expectedMetrics, func(i, j int) bool { return expectedMetrics[i].Less(expectedMetrics[j]) })

			// compare both to try to avoid go-difflib bug
			assert.Equal(t, expectedLines, actualLines)
			assert.Equal(t, expectedMetrics, actualMetrics)

			for i, v := range actualMetrics {
				switch v.Name {
				case "node_disk_read_bytes_total", "node_disk_written_bytes_total":
					assert.Equal(t, 2*v.Value, actualMetrics2[i].Value)
				default:
					assert.Equal(t, v.Value, actualMetrics2[i].Value)
				}
			}
		})
	}
}

func TestParseUptime(t *testing.T) {
	t.Skip("TODO Parse uptime https://jira.percona.com/browse/PMM-2131")

	_ = "01:45:58"
	_ = "1 day, 07:11:58"
}
