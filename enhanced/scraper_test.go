package enhanced

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/percona/exporter_shared/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/percona/rds_exporter/client"
	"github.com/percona/rds_exporter/config"
	"github.com/percona/rds_exporter/sessions"
)

func filterMetrics(metrics []*helpers.Metric) []*helpers.Metric {
	res := make([]*helpers.Metric, 0, len(metrics))
	processList := make(map[string]struct{})

	for _, m := range metrics {
		m.Value = 0

		// skip processList metrics that contain process IDs in labels that change too often
		if strings.Contains(m.Name, "_processList_") {
			if _, ok := processList[m.Name]; ok {
				continue
			}
			processList[m.Name] = struct{}{}
		}

		res = append(res, m)
	}
	return res
}

func TestScraper(t *testing.T) {
	cfg, err := config.Load("../config.tests.yml")
	require.NoError(t, err)
	client := client.New()
	sess, err := sessions.New(cfg.Instances, client.HTTP(), false)
	require.NoError(t, err)

	for session, instances := range sess.AllSessions() {
		session, instances := session, instances
		t.Run(fmt.Sprint(instances), func(t *testing.T) {
			// test that there are no new metrics
			s := newScraper(session, instances)
			s.testDisallowUnknownFields = true
			metrics, messages := s.scrape(context.Background())
			require.Len(t, metrics, len(instances))
			require.Len(t, messages, len(instances))

			for _, instance := range instances {
				// Test that actually received JSON matches expected JSON.
				// We can't do that directly, so we do it by comparing produced metrics
				// (minus values and processList metrics).

				instanceName := strings.TrimPrefix(instance.Instance, "autotest-")

				actualMetrics := helpers.ReadMetrics(metrics[instance.ResourceID])
				sort.Slice(actualMetrics, func(i, j int) bool { return actualMetrics[i].Less(actualMetrics[j]) })
				actualMetrics = filterMetrics(actualMetrics)
				actualLines := helpers.Format(helpers.WriteMetrics(actualMetrics))

				if *golden {
					writeTestDataJSON(t, instanceName, []byte(messages[instance.ResourceID]))
				}

				osMetrics, err := parseOSMetrics(readTestDataJSON(t, instanceName), true)
				require.NoError(t, err)
				expectedMetrics := helpers.ReadMetrics(osMetrics.makePrometheusMetrics(instance.Region, nil))
				sort.Slice(expectedMetrics, func(i, j int) bool { return expectedMetrics[i].Less(expectedMetrics[j]) })
				expectedMetrics = filterMetrics(expectedMetrics)
				expectedLines := helpers.Format(helpers.WriteMetrics(expectedMetrics))

				// compare both to try to avoid go-difflib bug
				assert.Equal(t, expectedLines, actualLines)
				assert.Equal(t, expectedMetrics, actualMetrics)
			}
		})
	}

	// if JSON was updated, update metrics too
	if !t.Failed() && *golden {
		*goldenTXT = true
		TestParse(t)
	}
}

func TestBetterTimes(t *testing.T) {
	type testdata struct {
		allTimes              map[string][]time.Time
		expectedTimes         map[string]time.Time
		expectedNextStartTime time.Time
	}
	for _, td := range []testdata{
		{
			allTimes: map[string][]time.Time{
				"1": {
					time.Date(2018, 9, 29, 16, 25, 42, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 26, 42, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 27, 42, 0, time.UTC),
				},
				"2": {
					time.Date(2018, 9, 29, 16, 25, 46, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 26, 46, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 27, 46, 0, time.UTC),
				},
				"3": {
					time.Date(2018, 9, 29, 16, 25, 51, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 26, 51, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 27, 51, 0, time.UTC),
				},
				"4": {
					time.Date(2018, 9, 29, 16, 26, 3, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 27, 3, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 28, 3, 0, time.UTC),
				},
			},
			expectedTimes: map[string]time.Time{
				"1": time.Date(2018, 9, 29, 16, 27, 42, 0, time.UTC),
				"2": time.Date(2018, 9, 29, 16, 27, 46, 0, time.UTC),
				"3": time.Date(2018, 9, 29, 16, 27, 51, 0, time.UTC),
				"4": time.Date(2018, 9, 29, 16, 28, 3, 0, time.UTC),
			},
			expectedNextStartTime: time.Date(2018, 9, 29, 16, 27, 42, 0, time.UTC),
		},
	} {
		times, nextStartTime := betterTimes(td.allTimes)
		assert.Equal(t, td.expectedTimes, times)
		assert.Equal(t, td.expectedNextStartTime, nextStartTime)
	}
}

func TestScraperDisableEnhancedMetrics(t *testing.T) {
	cfg, err := config.Load("../config.tests.yml")
	require.NoError(t, err)
	client := client.New()
	for i := range cfg.Instances {
		// Disable enhanced metrics in even instances.
		// This disable instance: no-such-instance.
		isDisabled := i%2 == 0
		cfg.Instances[i].DisableEnhancedMetrics = isDisabled
	}
	sess, err := sessions.New(cfg.Instances, client.HTTP(), false)
	require.NoError(t, err)

	// Check if all collected metrics do not contain metrics for instance with disabled metrics.
	hasMetricForInstance := func(lines []string, instanceName string) bool {
		for _, line := range lines {
			if strings.Contains(line, fmt.Sprintf("instance=%q", instanceName)) {
				return true
			}
		}
		return false
	}

	for session, instances := range sess.AllSessions() {
		session, instances := session, instances
		t.Run(fmt.Sprint(instances), func(t *testing.T) {
			s := newScraper(session, instances)
			s.testDisallowUnknownFields = true
			metrics, _ := s.scrape(context.Background())

			for _, instance := range instances {
				actualMetrics := helpers.ReadMetrics(metrics[instance.ResourceID])
				actualLines := helpers.Format(helpers.WriteMetrics(actualMetrics))
				name := instance.Instance
				if instance.DisableEnhancedMetrics {
					assert.Falsef(t, hasMetricForInstance(actualLines, name), "Found metrics for disabled instance %s", name)
					continue
				}
				assert.Truef(t, hasMetricForInstance(actualLines, name), "Did not find metrics for enabled instance %s", name)
			}
		})
	}
}
