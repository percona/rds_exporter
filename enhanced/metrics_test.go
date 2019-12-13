package enhanced

import (
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

			m, err := parseOSMetrics(readTestDataJSON(t, data.instance), true)
			require.NoError(t, err)

			actual := helpers.Format(m.makePrometheusMetrics(data.region, nil))

			if *goldenTXT {
				writeTestDataMetrics(t, data.instance, actual)
			}

			expected := readTestDataMetrics(t, data.instance)

			assert.Equal(t, expected, actual)
		})
	}
}

func TestParseUptime(t *testing.T) {
	t.Skip("TODO Parse uptime https://jira.percona.com/browse/PMM-2131")

	_ = "01:45:58"
	_ = "1 day, 07:11:58"
}
