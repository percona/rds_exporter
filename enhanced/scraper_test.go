package enhanced

import (
	"context"
	"fmt"
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
				// We can't do that directly, so we do it by comparing produced metrics (minus values).

				instanceName := strings.TrimPrefix(instance.Instance, "autotest-")

				actual := helpers.ReadMetrics(metrics[instance.ResourceID])
				for _, m := range actual {
					m.Value = 0
				}

				if *golden {
					writeTestDataJSON(t, instanceName, []byte(messages[instance.ResourceID]))
				}

				osMetrics, err := parseOSMetrics(readTestDataJSON(t, instanceName), true)
				require.NoError(t, err)
				expected := helpers.ReadMetrics(osMetrics.makePrometheusMetrics(instance.Region, nil))
				for _, m := range expected {
					m.Value = 0
				}

				assert.Equal(t, expected, actual)
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
