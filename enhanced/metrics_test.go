package enhanced

import (
	"bytes"
	"flag"
	"io/ioutil"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/percona/exporter_shared/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var golden = flag.Bool("golden", false, "update golden files")

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
	for _, data := range []struct {
		name      string
		region    string
		timestamp time.Time
	}{
		{"aurora-mysql-56", "us-east-1", time.Date(2019, 12, 12, 12, 31, 31, 0, time.UTC)},
		{"psql-10", "us-west-1", time.Date(2019, 12, 12, 12, 32, 17, 0, time.UTC)},
		{"mysql-57", "us-west-2", time.Date(2019, 12, 12, 12, 32, 13, 0, time.UTC)},
		{"aurora-psql-11", "us-west-2", time.Date(2019, 12, 12, 12, 32, 7, 0, time.UTC)},
	} {
		data := data
		t.Run(data.name, func(t *testing.T) {
			m, err := parseOSMetrics(readJSON(t, data.name+".json"))
			require.NoError(t, err)
			assert.Equal(t, data.timestamp, m.Timestamp)

			expected := readMetrics(t, data.name+".txt")
			actual := helpers.Format(m.makePrometheusMetrics(data.region, nil))
			actualS := strings.Join(actual, "\n")

			if *golden {
				expected = actual
				err = ioutil.WriteFile(filepath.Join("testdata", data.name+".txt"), []byte(actualS+"\n"), 0666)
				require.NoError(t, err)
			}

			assert.Equal(t, expected, actual, "Actual:\n%s", actualS)
		})
	}
}
