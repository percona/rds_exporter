package basic

import (
	"bytes"
	"flag"
	"io/ioutil"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	golden    = flag.Bool("golden", false, "does nothing; exists only for compatibility with other packages")
	goldenTXT = flag.Bool("golden-txt", false, "update golden .txt files")
)

func readTestDataMetrics(t *testing.T) []string {
	t.Helper()

	b, err := ioutil.ReadFile(filepath.Join("testdata", "all.txt")) //nolint:gosec
	require.NoError(t, err)
	return strings.Split(string(bytes.TrimSpace(b)), "\n")
}

func writeTestDataMetrics(t *testing.T, metrics []string) {
	t.Helper()

	b := []byte(strings.TrimSpace(strings.Join(metrics, "\n")) + "\n")
	err := ioutil.WriteFile(filepath.Join("testdata", "all.txt"), b, 0666)
	require.NoError(t, err)
}
