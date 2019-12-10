package enhanced

import (
	"bytes"
	"io/ioutil"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/percona/exporter_shared/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//nolint:lll
func readJSON(t *testing.T, file string) []byte {
	t.Helper()

	b, err := ioutil.ReadFile(filepath.Join("testdata", file)) //nolint:gosec
	require.NoError(t, err)
	return bytes.TrimSpace(b)
}

func readMetrics(t *testing.T, file string) []string {
	t.Helper()

	b, err := ioutil.ReadFile(filepath.Join("testdata", file)) //nolint:gosec
	require.NoError(t, err)
	return strings.Split(string(bytes.TrimSpace(b)), "\n")
}

func TestParse(t *testing.T) {
	t.Run("MySQL56", func(t *testing.T) {
		m, err := parseOSMetrics(readJSON(t, "mysql56.json"))
		require.NoError(t, err)
		assert.Equal(t, time.Date(2018, 10, 3, 10, 43, 5, 0, time.UTC), m.Timestamp)

		expected := readMetrics(t, "mysql56.txt")
		metrics := m.makePrometheusMetrics("us-east-1", nil)
		actual := helpers.Format(metrics)
		assert.Equal(t, expected, actual, "Actual:\n%s", strings.Join(actual, "\n"))
	})

	t.Run("MySQL57", func(t *testing.T) {
		m, err := parseOSMetrics(readJSON(t, "mysql57.json"))
		require.NoError(t, err)
		assert.Equal(t, time.Date(2018, 9, 25, 8, 7, 3, 0, time.UTC), m.Timestamp)

		expected := readMetrics(t, "mysql57.txt")
		metrics := m.makePrometheusMetrics("us-east-1", nil)
		actual := helpers.Format(metrics)
		assert.Equal(t, expected, actual, "Actual:\n%s", strings.Join(actual, "\n"))
	})

	t.Run("Aurora57", func(t *testing.T) {
		m, err := parseOSMetrics(readJSON(t, "aurora57.json"))
		require.NoError(t, err)
		assert.Equal(t, time.Date(2018, 9, 25, 8, 16, 20, 0, time.UTC), m.Timestamp)

		expected := readMetrics(t, "aurora57.txt")
		metrics := m.makePrometheusMetrics("us-east-1", nil)
		actual := helpers.Format(metrics)
		assert.Equal(t, expected, actual, "Actual:\n%s", strings.Join(actual, "\n"))
	})
}
