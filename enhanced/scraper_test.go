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

var errDescribeFailed = errors.New("describe failed")

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
			metrics, messages := s.scrape(t.Context())
			require.Len(t, metrics, len(instances))
			require.Len(t, messages, len(instances))

			for _, instance := range instances {
				// Test that actually received JSON matches expected JSON.
				// We can't do that directly, so we do it by comparing produced metrics
				// (minus values and processList metrics).

				instanceName := instance.Instance

				actualMetrics := helpers.ReadMetrics(metrics[keyOf(instance)].metrics)
				sort.Slice(actualMetrics, func(i, j int) bool { return actualMetrics[i].Less(actualMetrics[j]) })
				actualMetrics = filterMetrics(actualMetrics)
				actualLines := helpers.Format(helpers.WriteMetrics(actualMetrics))

				if *golden {
					writeTestDataJSON(t, instanceName, []byte(messages[keyOf(instance)]))
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

type fakeStateResolver struct {
	states map[string]sessions.InstanceState
	err    error
	calls  int
}

// InstanceStates returns the states and the error together, as the real resolver does when a page
// fails after earlier ones were read.
func (r *fakeStateResolver) InstanceStates(_ context.Context) (map[string]sessions.InstanceState, error) {
	r.calls++

	return r.states, r.err
}

func monitoredState(resourceID string) sessions.InstanceState {
	return sessions.InstanceState{ResourceID: resourceID, MonitoringInterval: time.Minute}
}

func newTestScraper(stateResolver instanceStateResolver) *scraper {
	return newTestScraperWith(nil, stateResolver, []sessions.Instance{
		testInstance(blueGreenPrimaryInstance, oldResourceID),
		testInstance(unchangedPrimaryInstance, sameResourceID),
	}, time.Time{})
}

// newTestScraperWithClient parks the resource ID refresh in the future so that only tests asking
// for a refresh exercise the resolver.
func newTestScraperWithClient(client cloudwatchlogs.FilterLogEventsAPIClient, instances []sessions.Instance) *scraper {
	resolver := &fakeStateResolver{states: nil, err: nil, calls: 0}

	return newTestScraperWith(client, resolver, instances, time.Now().Add(time.Hour))
}

func newTestScraperWith(
	client cloudwatchlogs.FilterLogEventsAPIClient,
	stateResolver instanceStateResolver,
	instances []sessions.Instance,
	nextResourceIDRefresh time.Time,
) *scraper {
	return &scraper{
		instances:                 instances,
		svc:                       client,
		stateResolver:             stateResolver,
		missing:                   newMissingStreams(),
		isolationCalls:            0,
		isolated:                  nil,
		batchAnswered:             false,
		groupProbeAfter:           time.Time{},
		errorCounts:               make(map[string]uint64),
		skewedEvents:              0,
		nextResourceIDRefresh:     nextResourceIDRefresh,
		refreshBackoff:            0,
		nextStartTime:             testEventTime().Add(-time.Minute),
		logger:                    promlog.New(&promlog.Config{}),
		testDisallowUnknownFields: false,
	}
}

func TestRegion(t *testing.T) {
	t.Parallel()

	assert.Equal(t, testRegion, scraperWithStreams(nil, oldResourceID).region())
	assert.Empty(t, scraperWithStreams(nil).region(), "a session without instances has no region to report under")
}

func TestMonitoredInstances(t *testing.T) {
	t.Parallel()

	unmonitored := testInstance("unmonitored", newResourceID)
	unmonitored.EnhancedMonitoringInterval = 0

	scraper := newTestScraperWithClient(nil, []sessions.Instance{
		testInstance(blueGreenPrimaryInstance, oldResourceID),
		// The same instance configured twice needs one entry, and one of the two having a stream is
		// enough for the collector to expect a sample.
		testInstance(blueGreenPrimaryInstance, oldResourceID),
		unmonitored,
	})

	assert.Equal(t, map[instanceKey]bool{
		testKey(blueGreenPrimaryInstance): true,
		testKey("unmonitored"):            false,
	}, scraper.result(nil).monitored)
}

func TestScrapeSkipsInstancesWithoutEnhancedMonitoring(t *testing.T) {
	t.Parallel()

	unmonitored := testInstance(unchangedPrimaryInstance, sameResourceID)
	unmonitored.EnhancedMonitoringInterval = 0

	client := &fakeLogsClient{
		events:   map[string][]types.FilteredLogEvent{oldResourceID: {osMetricsEvent(oldResourceID, testEventTime())}},
		missing:  map[string]struct{}{sameResourceID: {}},
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := newTestScraperWithClient(client, []sessions.Instance{
		testInstance(blueGreenPrimaryInstance, oldResourceID),
		unmonitored,
	})

	metrics, _ := scraper.scrape(t.Context())

	require.Len(t, client.calls, 1)
	assert.Equal(t, []string{oldResourceID}, client.calls[0].streams)
	assert.NotEmpty(t, metrics[testKey(blueGreenPrimaryInstance)])
	assert.Empty(t, metrics[testKey(unchangedPrimaryInstance)])
}

func TestScrapeFollowsTheMonitoringIntervalAWSReports(t *testing.T) {
	t.Parallel()

	unmonitored := testInstance(blueGreenPrimaryInstance, oldResourceID)
	unmonitored.EnhancedMonitoringInterval = 0

	resolver := &fakeStateResolver{
		states: map[string]sessions.InstanceState{
			blueGreenPrimaryInstance: {ResourceID: oldResourceID, MonitoringInterval: 5 * time.Second},
		},
		err:   nil,
		calls: 0,
	}
	client := &fakeLogsClient{events: nil, missing: nil, errs: nil, pageSize: 0, calls: nil}
	scraper := newTestScraperWith(client, resolver, []sessions.Instance{unmonitored}, time.Time{})

	require.Equal(t, maxInterval, scraper.interval(), "a session without Enhanced Monitoring has nothing to follow")

	scraper.scrape(t.Context())

	assert.Equal(t, 5*time.Second, scraper.interval(),
		"enabling Enhanced Monitoring must speed the scrapes up without a restart")
	assert.Equal(t, 5*time.Second, scraper.result(nil).interval, "the collector needs the interval to set expiry")
}

func TestRetune(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{events: nil, missing: nil, errs: nil, pageSize: 0, calls: nil}
	scraper := scraperWithStreams(client, oldResourceID)
	ticker := time.NewTicker(time.Hour)

	t.Cleanup(ticker.Stop)

	assert.Equal(t, time.Minute, scraper.retune(time.Hour, ticker), "the ticker follows the reported interval")
	assert.Equal(t, time.Minute, scraper.retune(time.Minute, ticker), "an unchanged interval leaves the ticker alone")
}

func TestRefreshUpdatesMonitoringInterval(t *testing.T) {
	t.Parallel()

	t.Run("enabled later", func(t *testing.T) {
		t.Parallel()

		unmonitored := testInstance(blueGreenPrimaryInstance, oldResourceID)
		unmonitored.EnhancedMonitoringInterval = 0
		resolver := &fakeStateResolver{
			states: map[string]sessions.InstanceState{blueGreenPrimaryInstance: monitoredState(oldResourceID)},
			err:    nil,
			calls:  0,
		}
		scraper := newTestScraperWith(nil, resolver, []sessions.Instance{unmonitored}, time.Time{})

		require.NoError(t, scraper.refreshInstanceStates(t.Context()))

		assert.Equal(t, time.Minute, scraper.instances[0].EnhancedMonitoringInterval)
		assert.Equal(t, []string{oldResourceID}, scraper.enhancedStreams(time.Now()))
	})

	t.Run("disabled later", func(t *testing.T) {
		t.Parallel()

		resolver := &fakeStateResolver{
			states: map[string]sessions.InstanceState{
				blueGreenPrimaryInstance: {ResourceID: oldResourceID, MonitoringInterval: 0},
			},
			err:   nil,
			calls: 0,
		}
		scraper := newTestScraperWith(nil, resolver, []sessions.Instance{
			testInstance(blueGreenPrimaryInstance, oldResourceID),
		}, time.Time{})

		require.NoError(t, scraper.refreshInstanceStates(t.Context()))

		assert.Zero(t, scraper.instances[0].EnhancedMonitoringInterval)
		assert.Empty(t, scraper.enhancedStreams(time.Now()))
	})
}

func TestRefreshKeepsMonitoringIntervalOnResolverError(t *testing.T) {
	t.Parallel()

	resolver := &fakeStateResolver{states: nil, err: errDescribeFailed, calls: 0}
	scraper := newTestScraper(resolver)

	require.ErrorIs(t, scraper.refreshInstanceStates(t.Context()), errDescribeFailed)

	assert.Equal(t, time.Minute, scraper.instances[0].EnhancedMonitoringInterval)
	assert.Equal(t, oldResourceID, scraper.instances[0].ResourceID)
	assert.Equal(t, []string{oldResourceID, sameResourceID}, scraper.enhancedStreams(time.Now()))
}

func TestScrapeCollectsEventsForEveryStream(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{
		events: map[string][]types.FilteredLogEvent{
			oldResourceID:  {osMetricsEvent(oldResourceID, testEventTime())},
			sameResourceID: {osMetricsEvent(sameResourceID, testEventTime())},
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
	assert.NotEmpty(t, metrics[testKey(blueGreenPrimaryInstance)])
	assert.NotEmpty(t, metrics[testKey(unchangedPrimaryInstance)])
	assert.Contains(t, messages[testKey(blueGreenPrimaryInstance)], oldResourceID)
}

func TestRefreshResourceIDs(t *testing.T) {
	t.Parallel()

	resolver := &fakeStateResolver{
		states: map[string]sessions.InstanceState{
			blueGreenPrimaryInstance: monitoredState(newResourceID),
			unchangedPrimaryInstance: monitoredState(sameResourceID),
		},
		err:   nil,
		calls: 0,
	}
	scraper := newTestScraper(resolver)

	err := scraper.refreshInstanceStates(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, resolver.calls)
	assert.Equal(t, newResourceID, scraper.instances[0].ResourceID)
	assert.Equal(t, sameResourceID, scraper.instances[1].ResourceID)
	assert.Equal(t, []string{newResourceID, sameResourceID}, scraper.enhancedStreams(time.Now()))
}

func TestRefreshResourceIDsReturnsResolverError(t *testing.T) {
	t.Parallel()

	resolver := &fakeStateResolver{states: nil, err: errDescribeFailed, calls: 0}
	scraper := newTestScraper(resolver)

	err := scraper.refreshInstanceStates(t.Context())

	require.ErrorIs(t, err, errDescribeFailed)
	assert.Equal(t, 1, resolver.calls)
	assert.Equal(t, oldResourceID, scraper.instances[0].ResourceID)
	assert.Equal(t, []string{oldResourceID, sameResourceID}, scraper.enhancedStreams(time.Now()))
	assert.Zero(t, scraper.missing.len(), "a resolver failure says nothing about which streams exist")
}

func TestRefreshAppliesPartialInstanceStates(t *testing.T) {
	t.Parallel()

	// One page read, the next one throttled.
	resolver := &fakeStateResolver{
		states: map[string]sessions.InstanceState{
			blueGreenPrimaryInstance: monitoredState(newResourceID),
		},
		err:   errDescribeFailed,
		calls: 0,
	}
	scraper := newTestScraper(resolver)
	scraper.missing.mark(oldResourceID, time.Now())

	err := scraper.refreshInstanceStates(t.Context())

	require.ErrorIs(t, err, errDescribeFailed, "the caller still has to hear that the refresh was partial")
	assert.Equal(t, newResourceID, scraper.instances[0].ResourceID,
		"a resource ID the resolver did read must not wait for the next refresh")
	assert.False(t, scraper.missing.marked(oldResourceID),
		"the retired stream must stop being excluded, or the switchover is written off as missing")
	assert.Equal(t, sameResourceID, scraper.instances[1].ResourceID,
		"an instance the failed page never reached keeps what it had")
	assert.Equal(t, []string{newResourceID, sameResourceID}, scraper.enhancedStreams(time.Now()))
}

func TestRefreshRetriesSoonerAfterAFailedRefresh(t *testing.T) {
	t.Parallel()

	resolver := &fakeStateResolver{states: nil, err: errDescribeFailed, calls: 0}
	scraper := newTestScraper(resolver)

	require.Error(t, scraper.refreshInstanceStates(t.Context()))

	// The instances the failed paginator never reached still hold whatever they had, so waiting the
	// whole interval leaves a retired log stream to be written off as missing meanwhile.
	assert.Equal(t, scraper.interval(), scraper.refreshBackoff,
		"the first retry is due on the next scrape that asks for one")

	scraper.nextResourceIDRefresh = time.Time{}
	require.Error(t, scraper.refreshInstanceStates(t.Context()))

	assert.Equal(t, 2*scraper.interval(), scraper.refreshBackoff,
		"a DescribeDBInstances that keeps failing must not be asked once per scrape")

	scraper.refreshBackoff = resourceIDRefreshInterval
	scraper.nextResourceIDRefresh = time.Time{}
	require.Error(t, scraper.refreshInstanceStates(t.Context()))

	assert.Equal(t, resourceIDRefreshInterval, scraper.refreshBackoff, "the backoff stops at the refresh interval")

	resolver.err = nil
	resolver.states = map[string]sessions.InstanceState{blueGreenPrimaryInstance: monitoredState(newResourceID)}
	scraper.nextResourceIDRefresh = time.Time{}
	require.NoError(t, scraper.refreshInstanceStates(t.Context()))

	assert.Zero(t, scraper.refreshBackoff)
	assert.InDelta(t, float64(resourceIDRefreshInterval), float64(time.Until(scraper.nextResourceIDRefresh)),
		float64(time.Second), "a refresh that read everything is not due again until the interval is up")
}

func TestRefreshResourceIDsSkipsMissingResourceID(t *testing.T) {
	t.Parallel()

	resolver := &fakeStateResolver{
		states: map[string]sessions.InstanceState{
			blueGreenPrimaryInstance: monitoredState(""),
			unchangedPrimaryInstance: monitoredState(sameResourceID),
		},
		err:   nil,
		calls: 0,
	}
	scraper := newTestScraper(resolver)

	err := scraper.refreshInstanceStates(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, resolver.calls)
	assert.Equal(t, oldResourceID, scraper.instances[0].ResourceID)
	assert.Equal(t, sameResourceID, scraper.instances[1].ResourceID)
	assert.Equal(t, []string{oldResourceID, sameResourceID}, scraper.enhancedStreams(time.Now()))
}

func TestRefreshResourceIDsNoopWhenUnchanged(t *testing.T) {
	t.Parallel()

	resolver := &fakeStateResolver{
		states: map[string]sessions.InstanceState{
			blueGreenPrimaryInstance: monitoredState(oldResourceID),
			unchangedPrimaryInstance: monitoredState(sameResourceID),
		},
		err:   nil,
		calls: 0,
	}
	scraper := newTestScraper(resolver)

	err := scraper.refreshInstanceStates(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, resolver.calls)
	assert.Equal(t, oldResourceID, scraper.instances[0].ResourceID)
	assert.Equal(t, sameResourceID, scraper.instances[1].ResourceID)
	assert.Equal(t, []string{oldResourceID, sameResourceID}, scraper.enhancedStreams(time.Now()))
}

func TestRefreshResourceIDsSkipsUntilNextRefresh(t *testing.T) {
	t.Parallel()

	resolver := &fakeStateResolver{
		states: map[string]sessions.InstanceState{
			blueGreenPrimaryInstance: monitoredState(newResourceID),
			unchangedPrimaryInstance: monitoredState(sameResourceID),
		},
		err:   nil,
		calls: 0,
	}
	scraper := newTestScraper(resolver)
	scraper.nextResourceIDRefresh = time.Now().Add(time.Minute)

	err := scraper.refreshInstanceStates(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 0, resolver.calls)
	assert.Equal(t, oldResourceID, scraper.instances[0].ResourceID)

	scraper.nextResourceIDRefresh = time.Now().Add(-time.Minute)

	err = scraper.refreshInstanceStates(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, resolver.calls)
	assert.Equal(t, newResourceID, scraper.instances[0].ResourceID)
	assert.Equal(t, []string{newResourceID, sameResourceID}, scraper.enhancedStreams(time.Now()))
}

// blockingStateResolver never answers, so the scrape deadline expires inside the refresh.
type blockingStateResolver struct {
	calls int
}

func (r *blockingStateResolver) InstanceStates(ctx context.Context) (map[string]sessions.InstanceState, error) {
	r.calls++

	<-ctx.Done()

	return nil, fmt.Errorf("fake RDS client: %w", ctx.Err())
}

func TestScrapeOnceSurvivesRefreshSpendingTheDeadline(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{
		events:   eventsFor(oldResourceID),
		missing:  nil,
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	resolver := &blockingStateResolver{calls: 0}
	scraper := newTestScraperWith(client, resolver,
		[]sessions.Instance{testInstance(oldResourceID, oldResourceID)}, time.Now().Add(-time.Minute))
	startTime := scraper.nextStartTime

	metrics := scraper.scrapeOnce(t.Context(), 50*time.Millisecond)

	assert.Empty(t, metrics)
	assert.Zero(t, scraper.missing.len(), "a scrape out of time must not mistake its own deadline for a missing stream")
	assert.Equal(t, startTime, scraper.nextStartTime, "the events left unread must not be skipped")
	assert.Equal(t, uint64(1), scraper.errorCounts[errorKindContext])

	// The refresh is not due again, and the next scrape gets its own deadline, so the cost of one
	// unresponsive DescribeDBInstances is a single empty scrape rather than a gap.
	scraper.errorCounts = make(map[string]uint64)

	metrics = scraper.scrapeOnce(t.Context(), time.Minute)

	assert.Equal(t, 1, resolver.calls)
	assert.NotEmpty(t, metrics[testKey(oldResourceID)])
}

func TestNewestEventTimes(t *testing.T) { //nolint:funlen
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	happened := now.Add(-time.Minute)
	future := now.Add(90 * time.Minute)

	type testdata struct {
		name                  string
		allTimes              map[instanceKey][]time.Time
		expectedTimes         map[instanceKey]time.Time
		expectedNextStartTime time.Time
		expectedCollected     bool
	}
	for _, td := range []testdata{
		{
			// The sink only holds instances an event arrived for, but an instance with nothing to judge
			// it by must not be given the zero time and must not decide where the next request starts.
			name: "an instance without events",
			allTimes: map[instanceKey][]time.Time{
				testKey("1"): {},
				testKey("2"): {happened},
			},
			expectedTimes: map[instanceKey]time.Time{
				testKey("2"): happened,
			},
			expectedNextStartTime: happened,
			expectedCollected:     true,
		},
		{
			// The raw timestamp decides which sample is newer, so one glitched event dated ahead of
			// the real ones would otherwise hold the instance's samples back until now caught up.
			name: "an event dated in the future alongside events that happened",
			allTimes: map[instanceKey][]time.Time{
				testKey("1"): {happened.Add(-time.Minute), future, happened},
			},
			expectedTimes: map[instanceKey]time.Time{
				testKey("1"): happened,
			},
			expectedNextStartTime: happened,
			expectedCollected:     true,
		},
		{
			// A host behind AWS dates every event ahead of itself. There is nothing else to judge the
			// instance by, so the newest still wins and the sample is still exported.
			name: "every event dated in the future",
			allTimes: map[instanceKey][]time.Time{
				testKey("1"): {future, future.Add(time.Second)},
			},
			expectedTimes: map[instanceKey]time.Time{
				testKey("1"): future.Add(time.Second),
			},
			expectedNextStartTime: future.Add(time.Second),
			expectedCollected:     true,
		},
		{
			name: "no events",
			// Nothing was collected, so the caller must keep its current start time.
			allTimes:              map[instanceKey][]time.Time{},
			expectedTimes:         map[instanceKey]time.Time{},
			expectedNextStartTime: time.Time{},
			expectedCollected:     false,
		},
		{
			name: "single instance",
			allTimes: map[instanceKey][]time.Time{
				testKey("1"): {
					time.Date(2018, 9, 29, 16, 25, 42, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 26, 42, 0, time.UTC),
				},
			},
			expectedTimes: map[instanceKey]time.Time{
				testKey("1"): time.Date(2018, 9, 29, 16, 26, 42, 0, time.UTC),
			},
			expectedNextStartTime: time.Date(2018, 9, 29, 16, 26, 42, 0, time.UTC),
			expectedCollected:     true,
		},
		{
			name: "duplicate timestamps",
			allTimes: map[instanceKey][]time.Time{
				testKey("1"): {
					time.Date(2018, 9, 29, 16, 25, 42, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 25, 42, 0, time.UTC),
				},
			},
			expectedTimes: map[instanceKey]time.Time{
				testKey("1"): time.Date(2018, 9, 29, 16, 25, 42, 0, time.UTC),
			},
			expectedNextStartTime: time.Date(2018, 9, 29, 16, 25, 42, 0, time.UTC),
			expectedCollected:     true,
		},
		{
			name: "oldest newest event across instances",
			allTimes: map[instanceKey][]time.Time{
				testKey("1"): {
					time.Date(2018, 9, 29, 16, 25, 42, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 26, 42, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 27, 42, 0, time.UTC),
				},
				testKey("2"): {
					time.Date(2018, 9, 29, 16, 25, 46, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 26, 46, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 27, 46, 0, time.UTC),
				},
				testKey("3"): {
					time.Date(2018, 9, 29, 16, 25, 51, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 26, 51, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 27, 51, 0, time.UTC),
				},
				testKey("4"): {
					time.Date(2018, 9, 29, 16, 26, 3, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 27, 3, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 28, 3, 0, time.UTC),
				},
			},
			expectedTimes: map[instanceKey]time.Time{
				testKey("1"): time.Date(2018, 9, 29, 16, 27, 42, 0, time.UTC),
				testKey("2"): time.Date(2018, 9, 29, 16, 27, 46, 0, time.UTC),
				testKey("3"): time.Date(2018, 9, 29, 16, 27, 51, 0, time.UTC),
				testKey("4"): time.Date(2018, 9, 29, 16, 28, 3, 0, time.UTC),
			},
			expectedNextStartTime: time.Date(2018, 9, 29, 16, 27, 42, 0, time.UTC),
			expectedCollected:     true,
		},
	} {
		t.Run(td.name, func(t *testing.T) {
			t.Parallel()

			times, nextStartTime, collected := newestEventTimes(td.allTimes, now)

			assert.Equal(t, td.expectedTimes, times)
			assert.Equal(t, td.expectedNextStartTime, nextStartTime)
			assert.Equal(t, td.expectedCollected, collected)
		})
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
			metrics, _ := s.scrape(t.Context())

			for _, instance := range instances {
				actualMetrics := helpers.ReadMetrics(metrics[keyOf(instance)].metrics)
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
