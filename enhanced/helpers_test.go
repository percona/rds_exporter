package enhanced

import (
	"bytes"
	"encoding/json"
	"flag"
	"io/ioutil"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	golden    = flag.Bool("golden", false, "update both golden .json and .txt files")
	goldenTXT = flag.Bool("golden-txt", false, "update golden .txt files")
)

func readTestDataJSON(t *testing.T, instance string) []byte {
	t.Helper()

	b, err := ioutil.ReadFile(filepath.Join("testdata", instance+".json")) //nolint:gosec
	require.NoError(t, err)
	return bytes.TrimSpace(b)
}

func writeTestDataJSON(t *testing.T, instance string, b []byte) {
	t.Helper()

	var buf bytes.Buffer
	err := json.Indent(&buf, b, "", "    ")
	require.NoError(t, err)
	err = buf.WriteByte('\n')
	require.NoError(t, err)
	err = ioutil.WriteFile(filepath.Join("testdata", instance+".json"), buf.Bytes(), 0666)
	require.NoError(t, err)
}

func readTestDataMetrics(t *testing.T, instance string) []string {
	t.Helper()

	b, err := ioutil.ReadFile(filepath.Join("testdata", instance+".txt")) //nolint:gosec
	require.NoError(t, err)
	return strings.Split(string(bytes.TrimSpace(b)), "\n")
}

func writeTestDataMetrics(t *testing.T, instance string, metrics []string) {
	t.Helper()

	b := []byte(strings.TrimSpace(strings.Join(metrics, "\n")) + "\n")
	err := ioutil.WriteFile(filepath.Join("testdata", instance+".txt"), b, 0666)
	require.NoError(t, err)
}
