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

func readJSON(t *testing.T, instance string) []byte {
	t.Helper()

	b, err := ioutil.ReadFile(filepath.Join("testdata", instance+".json")) //nolint:gosec
	require.NoError(t, err)
	return bytes.TrimSpace(b)
}

func readMetrics(t *testing.T, instance string) []string {
	t.Helper()

	b, err := ioutil.ReadFile(filepath.Join("testdata", instance+".txt")) //nolint:gosec
	require.NoError(t, err)
	return strings.Split(string(bytes.TrimSpace(b)), "\n")
}

func TestParse(t *testing.T) {
	for _, data := range []struct {
		region    string
		instance  string
		timestamp time.Time
	}{
		{"us-east-1", "aurora-mysql-56", time.Date(2019, 12, 12, 12, 31, 31, 0, time.UTC)},
		{"us-west-1", "psql-10", time.Date(2019, 12, 12, 12, 32, 17, 0, time.UTC)},
		{"us-west-2", "mysql-57", time.Date(2019, 12, 12, 12, 32, 13, 0, time.UTC)},
		{"us-west-2", "aurora-psql-11", time.Date(2019, 12, 12, 12, 32, 7, 0, time.UTC)},
	} {
		data := data
		t.Run(data.instance, func(t *testing.T) {
			// do not update files concurrently
			if !*golden {
				t.Parallel()
			}

			m, err := parseOSMetrics(readJSON(t, data.instance), true)
			require.NoError(t, err)
			assert.Equal(t, data.timestamp, m.Timestamp)

			expected := readMetrics(t, data.instance)
			actual := helpers.Format(m.makePrometheusMetrics(data.region, nil))
			actualS := strings.Join(actual, "\n")

			if *golden {
				expected = actual
				err = ioutil.WriteFile(filepath.Join("testdata", data.instance), []byte(actualS+"\n"), 0666)
				require.NoError(t, err)
			}

			assert.Equal(t, expected, actual, "Actual:\n%s", actualS)
		})
	}
}
