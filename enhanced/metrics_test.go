package enhanced

import (
	"strings"
	"testing"
	"time"

	"github.com/percona/exporter_shared/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//nolint:lll
func TestParse(t *testing.T) {
	t.Run("MySQL56", func(t *testing.T) {
		m, err := parseOSMetrics(dataMySQL56)
		require.NoError(t, err)
		assert.Equal(t, time.Date(2018, 10, 3, 10, 43, 5, 0, time.UTC), m.Timestamp)

		metrics := m.makePrometheusMetrics("us-east-1", nil)
		actual := strings.Join(helpers.Format(metrics), "\n")
		assert.Equal(t, dataMySQL56Expected, actual, "Actual:\n%s", actual)
	})

	t.Run("MySQL57", func(t *testing.T) {
		m, err := parseOSMetrics(dataMySQL57)
		require.NoError(t, err)
		assert.Equal(t, time.Date(2018, 9, 25, 8, 7, 3, 0, time.UTC), m.Timestamp)

		metrics := m.makePrometheusMetrics("us-east-1", nil)
		actual := strings.Join(helpers.Format(metrics), "\n")
		assert.Equal(t, dataMySQL57Expected, actual, "Actual:\n%s", actual)
	})

	t.Run("Aurora57", func(t *testing.T) {
		m, err := parseOSMetrics(dataAurora57)
		require.NoError(t, err)
		assert.Equal(t, time.Date(2018, 9, 25, 8, 16, 20, 0, time.UTC), m.Timestamp)

		metrics := m.makePrometheusMetrics("us-east-1", nil)
		actual := strings.Join(helpers.Format(metrics), "\n")
		assert.Equal(t, dataAurora57Expected, actual, "Actual:\n%s", actual)
	})
}
