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

			actualMetrics := helpers.ReadMetrics(m.makePrometheusMetrics(data.region, data.instance, nil))
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

func TestMakePrometheusMetrics(t *testing.T) {
	t.Parallel()

	const configured = "prod-db"

	for _, testCase := range []struct {
		name     string
		payload  string
		labels   map[string]string
		instance string
		labelled bool
	}{
		{name: "names the instance from the configuration, not a renamed box", payload: configured + "-old1",
			labels: nil, instance: configured, labelled: true},
		{name: "names the instance when the document names none", payload: "",
			labels: nil, instance: configured, labelled: true},
		{name: "lets a configured label override the name", payload: configured + "-old1",
			labels: map[string]string{instanceLabel: "custom"}, instance: "custom", labelled: true},
		{name: "lets an empty configured label drop the name", payload: configured + "-old1",
			labels: map[string]string{instanceLabel: ""}, instance: "", labelled: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			m, err := parseOSMetrics([]byte(osMetricsDocument(oldResourceID, testCase.payload, testEventTime())), true)
			require.NoError(t, err)

			metrics := m.makePrometheusMetrics(testRegion, configured, testCase.labels)
			require.NotEmpty(t, metrics)

			for _, metric := range helpers.ReadMetrics(metrics) {
				instance, labelled := metric.Labels[instanceLabel]
				assert.Equal(t, testCase.labelled, labelled, "%s", metric.Name)
				assert.Equal(t, testCase.instance, instance, "%s", metric.Name)
			}
		})
	}
}

func TestParseUptime(t *testing.T) {
	t.Skip("uptime is not parsed yet")

	_ = "01:45:58"
	_ = "1 day, 07:11:58"
}
