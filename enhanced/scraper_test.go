package enhanced

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/percona/exporter_shared/helpers"
	"github.com/prometheus/common/promlog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/percona/rds_exporter/client"
	"github.com/percona/rds_exporter/config"
	"github.com/percona/rds_exporter/sessions"
)

const (
	blueGreenPrimaryInstance = "blue-green-primary"
	newResourceID            = "new-resource-id"
	oldResourceID            = "old-resource-id"
	sameResourceID           = "same-resource-id"
	unchangedPrimaryInstance = "unchanged-primary"
	testRegion               = "us-east-1"
)

var (
	errDescribeFailed = errors.New("describe failed")

	testEventTime = time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
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
	logger := promlog.New(&promlog.Config{})
	client := client.New(logger)
	sess, err := sessions.New(cfg.Instances, client.HTTP(), logger, false)
	require.NoError(t, err)

	for session, instances := range sess.AllSessions() {
		t.Run(fmt.Sprint(instances), func(t *testing.T) {
			cfg := sess.Configs[session]
			s := newScraper(cfg, instances, logger)
			s.testDisallowUnknownFields = true
			metrics, messages := s.scrape(context.Background())
			require.Len(t, metrics, len(instances))
			require.Len(t, messages, len(instances))

			for _, instance := range instances {
				// Test that actually received JSON matches expected JSON.
				// We can't do that directly, so we do it by comparing produced metrics
				// (minus values and processList metrics).

				instanceName := instance.Instance

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

type fakeResourceIDResolver struct {
	resourceIDs map[string]string
	err         error
	calls       int
}

func (r *fakeResourceIDResolver) ResourceIDs(_ context.Context) (map[string]string, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}

	return r.resourceIDs, nil
}

func newTestScraper(resourceIDResolver resourceIDResolver) *scraper {
	logger := promlog.New(&promlog.Config{})
	return &scraper{
		instances: []sessions.Instance{
			{
				Region:                     "us-east-1",
				Instance:                   blueGreenPrimaryInstance,
				DisableBasicMetrics:        false,
				DisableEnhancedMetrics:     false,
				ResourceID:                 oldResourceID,
				Labels:                     nil,
				EnhancedMonitoringInterval: 0,
			},
			{
				Region:                     "us-east-1",
				Instance:                   unchangedPrimaryInstance,
				DisableBasicMetrics:        false,
				DisableEnhancedMetrics:     false,
				ResourceID:                 sameResourceID,
				Labels:                     nil,
				EnhancedMonitoringInterval: 0,
			},
		},
		logStreamNames:            []string{oldResourceID, sameResourceID},
		svc:                       nil,
		resourceIDResolver:        resourceIDResolver,
		nextResourceIDRefresh:     time.Time{},
		nextStartTime:             time.Time{},
		logger:                    logger,
		testDisallowUnknownFields: false,
	}
}

// newTestScraperWithClient returns a scraper backed by a fake CloudWatch Logs client, with the
// resource ID refresh parked in the future so only tests that ask for it exercise the resolver.
func newTestScraperWithClient(client cloudwatchlogs.FilterLogEventsAPIClient, instances []sessions.Instance) *scraper {
	logStreamNames := make([]string, 0, len(instances))
	for _, instance := range instances {
		logStreamNames = append(logStreamNames, instance.ResourceID)
	}

	return &scraper{
		instances:                 instances,
		logStreamNames:            logStreamNames,
		svc:                       client,
		resourceIDResolver:        &fakeResourceIDResolver{resourceIDs: nil, err: nil, calls: 0},
		nextResourceIDRefresh:     time.Now().Add(time.Hour),
		nextStartTime:             testEventTime.Add(-time.Minute),
		logger:                    promlog.New(&promlog.Config{}),
		testDisallowUnknownFields: false,
	}
}

func TestScrapeCollectsEventsForEveryStream(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{
		events: map[string][]types.FilteredLogEvent{
			oldResourceID:  {osMetricsEvent(oldResourceID, testEventTime)},
			sameResourceID: {osMetricsEvent(sameResourceID, testEventTime)},
		},
		missing:  nil,
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := newTestScraperWithClient(client, []sessions.Instance{
		testInstance(blueGreenPrimaryInstance, oldResourceID),
		testInstance(unchangedPrimaryInstance, sameResourceID),
	})

	metrics, messages := scraper.scrape(t.Context())

	require.Len(t, client.calls, 1)
	assert.Equal(t, []string{oldResourceID, sameResourceID}, client.calls[0].streams)
	assert.NotEmpty(t, metrics[oldResourceID])
	assert.NotEmpty(t, metrics[sameResourceID])
	assert.Contains(t, messages[oldResourceID], oldResourceID)
}

func TestRefreshResourceIDs(t *testing.T) {
	t.Parallel()

	resolver := &fakeResourceIDResolver{
		resourceIDs: map[string]string{
			blueGreenPrimaryInstance: newResourceID,
			unchangedPrimaryInstance: sameResourceID,
		},
		err:   nil,
		calls: 0,
	}
	scraper := newTestScraper(resolver)

	err := scraper.refreshResourceIDs(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, resolver.calls)
	assert.Equal(t, newResourceID, scraper.instances[0].ResourceID)
	assert.Equal(t, newResourceID, scraper.logStreamNames[0])
	assert.Equal(t, sameResourceID, scraper.instances[1].ResourceID)
	assert.Equal(t, sameResourceID, scraper.logStreamNames[1])
}

func TestRefreshResourceIDsReturnsResolverError(t *testing.T) {
	t.Parallel()

	resolver := &fakeResourceIDResolver{
		resourceIDs: nil,
		err:         errDescribeFailed,
		calls:       0,
	}
	scraper := newTestScraper(resolver)

	err := scraper.refreshResourceIDs(t.Context())

	require.ErrorIs(t, err, errDescribeFailed)
	assert.Equal(t, 1, resolver.calls)
	assert.Equal(t, oldResourceID, scraper.instances[0].ResourceID)
	assert.Equal(t, oldResourceID, scraper.logStreamNames[0])
}

func TestRefreshResourceIDsSkipsMissingResourceID(t *testing.T) {
	t.Parallel()

	resolver := &fakeResourceIDResolver{
		resourceIDs: map[string]string{
			blueGreenPrimaryInstance: "",
			unchangedPrimaryInstance: sameResourceID,
		},
		err:   nil,
		calls: 0,
	}
	scraper := newTestScraper(resolver)

	err := scraper.refreshResourceIDs(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, resolver.calls)
	assert.Equal(t, oldResourceID, scraper.instances[0].ResourceID)
	assert.Equal(t, oldResourceID, scraper.logStreamNames[0])
	assert.Equal(t, sameResourceID, scraper.instances[1].ResourceID)
	assert.Equal(t, sameResourceID, scraper.logStreamNames[1])
}

func TestRefreshResourceIDsNoopWhenUnchanged(t *testing.T) {
	t.Parallel()

	resolver := &fakeResourceIDResolver{
		resourceIDs: map[string]string{
			blueGreenPrimaryInstance: oldResourceID,
			unchangedPrimaryInstance: sameResourceID,
		},
		err:   nil,
		calls: 0,
	}
	scraper := newTestScraper(resolver)

	err := scraper.refreshResourceIDs(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, resolver.calls)
	assert.Equal(t, oldResourceID, scraper.instances[0].ResourceID)
	assert.Equal(t, oldResourceID, scraper.logStreamNames[0])
	assert.Equal(t, sameResourceID, scraper.instances[1].ResourceID)
	assert.Equal(t, sameResourceID, scraper.logStreamNames[1])
}

func TestRefreshResourceIDsSkipsUntilNextRefresh(t *testing.T) {
	t.Parallel()

	resolver := &fakeResourceIDResolver{
		resourceIDs: map[string]string{
			blueGreenPrimaryInstance: newResourceID,
			unchangedPrimaryInstance: sameResourceID,
		},
		err:   nil,
		calls: 0,
	}
	scraper := newTestScraper(resolver)
	scraper.nextResourceIDRefresh = time.Now().Add(time.Minute)

	err := scraper.refreshResourceIDs(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 0, resolver.calls)
	assert.Equal(t, oldResourceID, scraper.instances[0].ResourceID)
	assert.Equal(t, oldResourceID, scraper.logStreamNames[0])

	scraper.nextResourceIDRefresh = time.Now().Add(-time.Minute)

	err = scraper.refreshResourceIDs(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, resolver.calls)
	assert.Equal(t, newResourceID, scraper.instances[0].ResourceID)
	assert.Equal(t, newResourceID, scraper.logStreamNames[0])
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
	logger := promlog.New(&promlog.Config{})
	client := client.New(logger)
	for i := range cfg.Instances {
		// Disable enhanced metrics in even instances.
		// This disable instance: no-such-instance.
		isDisabled := i%2 == 0
		cfg.Instances[i].DisableEnhancedMetrics = isDisabled
	}
	sess, err := sessions.New(cfg.Instances, client.HTTP(), logger, false)
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
		t.Run(fmt.Sprint(instances), func(t *testing.T) {
			s := newScraper(sess.Configs[session], instances, logger)
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
