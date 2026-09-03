package enhanced

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/percona/rds_exporter/sessions"
)

const missingResourceID = "missing-resource-id"

// scraperWithStreams returns a scraper for instances named after their own resource ID, which keeps
// batch-shape assertions readable.
func scraperWithStreams(client *fakeLogsClient, resourceIDs ...string) *scraper {
	instances := make([]sessions.Instance, 0, len(resourceIDs))
	for _, resourceID := range resourceIDs {
		instances = append(instances, testInstance(resourceID, resourceID))
	}

	return newTestScraperWithClient(client, instances)
}

func resourceIDs(count int) []string {
	res := make([]string, 0, count)
	for i := range count {
		res = append(res, fmt.Sprintf("db-%03d", i))
	}

	return res
}

func eventsFor(resourceIDs ...string) map[string][]types.FilteredLogEvent {
	res := make(map[string][]types.FilteredLogEvent, len(resourceIDs))
	for _, resourceID := range resourceIDs {
		res[resourceID] = []types.FilteredLogEvent{osMetricsEvent(resourceID, testEventTime())}
	}

	return res
}

func TestScrapeIsolatesMissingLogStream(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{
		events:   eventsFor(oldResourceID, sameResourceID),
		missing:  map[string]struct{}{missingResourceID: {}},
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := scraperWithStreams(client, oldResourceID, missingResourceID, sameResourceID)

	metrics, _ := scraper.scrape(t.Context())

	assert.NotEmpty(t, metrics[testKey(oldResourceID)], "a missing stream must not starve the instances before it")
	assert.NotEmpty(t, metrics[testKey(sameResourceID)], "a missing stream must not starve the instances after it")
	assert.Empty(t, metrics[testKey(missingResourceID)])

	client.calls = nil
	metrics, _ = scraper.scrape(t.Context())

	require.Len(t, client.calls, 1, "a known missing stream must not cost an extra request")
	assert.Equal(t, []string{oldResourceID, sameResourceID}, client.calls[0].streams)
	assert.NotEmpty(t, metrics[testKey(oldResourceID)])
	assert.NotEmpty(t, metrics[testKey(sameResourceID)])
}

func TestScrapeCountsMissingStreamOnce(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{
		events:   eventsFor(oldResourceID),
		missing:  map[string]struct{}{missingResourceID: {}},
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := scraperWithStreams(client, oldResourceID, missingResourceID)

	scraper.scrape(t.Context())

	assert.Equal(t, uint64(1), scraper.errorCounts[errorKindNotFound],
		"a missing stream must be visible as an error, not silently swallowed")

	scraper.errorCounts = make(map[string]uint64)
	scraper.scrape(t.Context())

	assert.Zero(t, scraper.errorCounts[errorKindNotFound],
		"an already excluded stream must not inflate the counter on every scrape")
}

func TestScrapeReprobesMissingStream(t *testing.T) { //nolint:funlen
	t.Parallel()

	newClient := func() *fakeLogsClient {
		return &fakeLogsClient{
			events:   eventsFor(oldResourceID, missingResourceID),
			missing:  map[string]struct{}{missingResourceID: {}},
			errs:     nil,
			pageSize: 0,
			calls:    nil,
		}
	}

	t.Run("not until the probe is due", func(t *testing.T) {
		t.Parallel()

		client := newClient()
		scraper := scraperWithStreams(client, oldResourceID, missingResourceID)
		scraper.scrape(t.Context())

		client.calls = nil

		scraper.scrape(t.Context())

		require.Len(t, client.calls, 1)
		assert.Equal(t, []string{oldResourceID}, client.calls[0].streams)
	})

	t.Run("still missing when the probe comes due", func(t *testing.T) {
		t.Parallel()

		client := newClient()
		scraper := scraperWithStreams(client, oldResourceID, missingResourceID)
		scraper.scrape(t.Context())

		scraper.missing.probeAfter[missingResourceID] = time.Now().Add(-time.Minute)
		scraper.errorCounts = make(map[string]uint64)

		scraper.scrape(t.Context())

		assert.Equal(t, 1, scraper.missing.len(), "a stream that is still missing stays excluded")
		assert.Zero(t, scraper.errorCounts[errorKindNotFound], "a stream already excluded must not be reported again")
		assert.True(t, scraper.missing.probeAfter[missingResourceID].After(time.Now()),
			"a failed probe must wait another TTL")
	})

	t.Run("heals once the stream exists", func(t *testing.T) {
		t.Parallel()

		client := newClient()
		scraper := scraperWithStreams(client, oldResourceID, missingResourceID)
		scraper.scrape(t.Context())

		delete(client.missing, missingResourceID)
		scraper.missing.probeAfter[missingResourceID] = time.Now().Add(-time.Minute)
		client.calls = nil

		metrics, _ := scraper.scrape(t.Context())

		assert.NotEmpty(t, metrics[testKey(missingResourceID)])
		assert.Zero(t, scraper.missing.len())
	})
}

func TestScrapeStopsExcludingSilentStream(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{
		events:   eventsFor(oldResourceID),
		missing:  map[string]struct{}{missingResourceID: {}},
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := scraperWithStreams(client, oldResourceID, missingResourceID)
	scraper.scrape(t.Context())
	require.Equal(t, 1, scraper.missing.len())

	// The stream exists again, but has published nothing inside the request window.
	delete(client.missing, missingResourceID)
	scraper.missing.probeAfter[missingResourceID] = time.Now().Add(-time.Minute)

	scraper.scrape(t.Context())

	assert.Zero(t, scraper.missing.len(),
		"CloudWatch answering the request proves the stream exists, whether or not it published")

	client.calls = nil

	scraper.scrape(t.Context())

	require.Len(t, client.calls, 1)
	assert.Equal(t, []string{oldResourceID, missingResourceID}, client.calls[0].streams,
		"a stream that exists must be requested again instead of waiting for another probe")
}

func TestScrapeStopsExcludingStreamAnsweredBeforeAPageFailed(t *testing.T) {
	t.Parallel()

	// A scraper that has already excluded a stream which exists again and is due to be probed.
	newScraper := func(t *testing.T) (*fakeLogsClient, *scraper) {
		t.Helper()

		client := &fakeLogsClient{
			events:   eventsFor(oldResourceID, missingResourceID),
			missing:  map[string]struct{}{missingResourceID: {}},
			errs:     nil,
			pageSize: 0,
			calls:    nil,
		}
		scraper := scraperWithStreams(client, oldResourceID, missingResourceID)
		scraper.scrape(t.Context())
		require.Equal(t, 1, scraper.missing.len())

		delete(client.missing, missingResourceID)
		scraper.missing.probeAfter[missingResourceID] = time.Now().Add(-time.Minute)

		return client, scraper
	}

	t.Run("clears the streams the answered page listed", func(t *testing.T) {
		t.Parallel()

		client, scraper := newScraper(t)
		client.pageSize = 1
		client.errs = []error{nil, throttlingError()}

		metrics, _ := scraper.scrape(t.Context())

		assert.Zero(t, scraper.missing.len(),
			"a page CloudWatch answered proves the streams it listed exist, whatever a later page does")
		assert.NotEmpty(t, metrics, "events already read must survive a later page error")
	})

	t.Run("keeps excluding when the request is rejected", func(t *testing.T) {
		t.Parallel()

		client, scraper := newScraper(t)
		probeAfter := scraper.missing.probeAfter[missingResourceID]
		client.errs = []error{throttlingError()}

		scraper.scrape(t.Context())

		assert.Equal(t, 1, scraper.missing.len(), "a request CloudWatch rejected proves nothing")
		assert.Equal(t, probeAfter, scraper.missing.probeAfter[missingResourceID],
			"only a rejection may re-arm the probe, so the stream stays due on the next scrape")
	})
}

func TestScrapeSpendsOneProbeSlotPerLogStream(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{
		events:   eventsFor(sameResourceID),
		missing:  map[string]struct{}{oldResourceID: {}, missingResourceID: {}},
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}

	// One stream configured more often than there are probe slots, so counting per instance would
	// leave none for the other stream.
	instances := make([]sessions.Instance, 0, maxProbesPerScrape+3)
	for i := range maxProbesPerScrape + 1 {
		instances = append(instances, testInstance(fmt.Sprintf("duplicate-%d", i), oldResourceID))
	}
	instances = append(instances, testInstance("other", missingResourceID))
	// A stream that answers, so the rejection is attributed to the streams rather than to the group.
	instances = append(instances, testInstance("healthy", sameResourceID))

	scraper := newTestScraperWithClient(client, instances)
	scraper.scrape(t.Context())
	require.Equal(t, 2, scraper.missing.len())

	client.missing = map[string]struct{}{}
	for _, stream := range []string{oldResourceID, missingResourceID} {
		scraper.missing.probeAfter[stream] = time.Now().Add(-time.Minute)
	}
	client.calls = nil

	scraper.scrape(t.Context())

	require.Len(t, client.calls, 1)
	assert.Equal(t, []string{oldResourceID, missingResourceID, sameResourceID}, client.calls[0].streams,
		"duplicate instances must neither repeat their stream nor spend another stream's probe slot")
}

func TestScrapeClearsMissingStreamWhenMonitoringIsDisabled(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{
		events:   nil,
		missing:  map[string]struct{}{oldResourceID: {}},
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := newTestScraperWithClient(client, []sessions.Instance{
		testInstance(blueGreenPrimaryInstance, oldResourceID),
	})
	scraper.scrape(t.Context())
	require.Equal(t, 1, scraper.missing.len())

	scraper.stateResolver = &fakeStateResolver{
		states: map[string]sessions.InstanceState{
			blueGreenPrimaryInstance: {ResourceID: oldResourceID, MonitoringInterval: 0},
		},
		err:   nil,
		calls: 0,
	}
	scraper.nextResourceIDRefresh = time.Now().Add(-time.Minute)

	scraper.scrape(t.Context())

	assert.Zero(t, scraper.missing.len(),
		"an exclusion must not outlive the log stream it excludes")
}

func TestScrapeClearsMissingStreamOnResourceIDChange(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{
		events:   eventsFor(newResourceID),
		missing:  map[string]struct{}{oldResourceID: {}},
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := newTestScraperWithClient(client, []sessions.Instance{
		testInstance(blueGreenPrimaryInstance, oldResourceID),
	})
	scraper.scrape(t.Context())
	require.Equal(t, 1, scraper.missing.len())

	scraper.stateResolver = &fakeStateResolver{
		states: map[string]sessions.InstanceState{blueGreenPrimaryInstance: monitoredState(newResourceID)},
		err:    nil,
		calls:  0,
	}
	scraper.nextResourceIDRefresh = time.Now().Add(-time.Minute)
	client.calls = nil

	metrics, _ := scraper.scrape(t.Context())

	assert.Zero(t, scraper.missing.len(), "the retired resource ID must not stay in the missing set")
	require.Len(t, client.calls, 1)
	assert.Equal(t, []string{newResourceID}, client.calls[0].streams)
	assert.NotEmpty(t, metrics[testKey(blueGreenPrimaryInstance)])
}

func TestScrapeKeepsStreamsOnRecoverableError(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name string
		err  error
		kind string
	}{
		{name: "throttling", err: throttlingError(), kind: errorKindThrottling},
		{
			name: "expired credentials",
			err:  &smithy.GenericAPIError{Code: "ExpiredTokenException", Message: "expired", Fault: 0},
			kind: errorKindAuth,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			client := &fakeLogsClient{
				events:   eventsFor(oldResourceID),
				missing:  nil,
				errs:     []error{testCase.err},
				pageSize: 0,
				calls:    nil,
			}
			scraper := scraperWithStreams(client, oldResourceID)

			metrics, _ := scraper.scrape(t.Context())

			assert.Empty(t, metrics)
			assert.Zero(t, scraper.missing.len(), "only a missing stream may be excluded")
			assert.Equal(t, testCase.kind, errorKind(testCase.err))
		})
	}
}

func TestScrapeKeepsPartialResults(t *testing.T) {
	t.Parallel()

	t.Run("when a later page fails", func(t *testing.T) {
		t.Parallel()

		client := &fakeLogsClient{
			events:   eventsFor(oldResourceID, sameResourceID),
			missing:  nil,
			errs:     []error{nil, throttlingError()},
			pageSize: 1,
			calls:    nil,
		}
		scraper := scraperWithStreams(client, oldResourceID, sameResourceID)

		metrics, _ := scraper.scrape(t.Context())

		assert.NotEmpty(t, metrics[testKey(oldResourceID)], "events already read must survive a later page error")
	})

	t.Run("when an earlier batch fails", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(150)
		client := &fakeLogsClient{
			events:   eventsFor(streams[len(streams)-1]),
			missing:  nil,
			errs:     []error{throttlingError()},
			pageSize: 0,
			calls:    nil,
		}
		scraper := scraperWithStreams(client, streams...)

		metrics, _ := scraper.scrape(t.Context())

		assert.Len(t, client.calls, 2)
		assert.NotEmpty(t, metrics[testKey(streams[len(streams)-1])], "a failed batch must not skip the batches after it")
	})
}

func TestScrapeKeepsIsolatingWhenAHalfFailsForAnotherReason(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(4)
	missing := streams[len(streams)-1]
	healthy := streams[len(streams)-2]

	// The batch is rejected, and the first half it is cut into is throttled instead of answered.
	client := &fakeLogsClient{
		events:   eventsFor(healthy),
		missing:  map[string]struct{}{missing: {}},
		errs:     []error{nil, throttlingError()},
		pageSize: 0,
		calls:    nil,
	}
	scraper := scraperWithStreams(client, streams...)
	startTime := scraper.nextStartTime

	metrics, _ := scraper.scrape(t.Context())

	assert.NotEmpty(t, metrics[testKey(healthy)], "a throttled half must not stop the other half from reporting")
	assert.True(t, scraper.missing.marked(missing))
	assert.Equal(t, 1, scraper.missing.len(),
		"only a rejection may exclude a stream, so a throttled half stays in the request")
	assert.Equal(t, uint64(1), scraper.errorCounts[errorKindThrottling])
	assert.Equal(t, uint64(1), scraper.errorCounts[errorKindNotFound])
	assert.Equal(t, startTime, scraper.nextStartTime, "the half that was throttled must be read again")

	client.calls = nil

	scraper.scrape(t.Context())

	require.Len(t, client.calls, 1)
	assert.Equal(t, streams[:len(streams)-1], client.calls[0].streams,
		"the throttled streams must be retried on the next scrape, without the excluded one")
}

func TestScrapeProbesEveryMissingStreamAcrossScrapes(t *testing.T) {
	t.Parallel()

	const rounds = 4

	streams := resourceIDs(rounds * maxProbesPerScrape)

	missing := make(map[string]struct{}, len(streams))
	for _, stream := range streams {
		missing[stream] = struct{}{}
	}

	// A stream that answers, so the rejection is attributed to the streams rather than to the group.
	client := &fakeLogsClient{
		events:   eventsFor(sameResourceID),
		missing:  missing,
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := scraperWithStreams(client, append(streams, sameResourceID)...)

	for _, stream := range streams {
		scraper.missing.mark(stream, time.Now().Add(-2*missingStreamTTL))
	}

	probed := make([]string, 0, len(streams))

	for range rounds {
		client.calls = nil

		scraper.scrape(t.Context())

		require.NotEmpty(t, client.calls)
		require.Len(t, client.calls[0].streams, maxProbesPerScrape+1)

		probed = append(probed, client.calls[0].streams[:maxProbesPerScrape]...)
	}

	assert.ElementsMatch(t, streams, probed,
		"a failed probe waits another TTL, so the first slots cannot keep the streams behind them from being retried")
}

func TestScrapeReportsIsolationBudgetExhaustion(t *testing.T) {
	t.Parallel()

	newScraper := func() *scraper {
		streams := resourceIDs(maxLogStreamsPerRequest)

		missing := make(map[string]struct{}, len(streams))
		for _, stream := range streams {
			missing[stream] = struct{}{}
		}

		client := &fakeLogsClient{events: nil, missing: missing, errs: nil, pageSize: 0, calls: nil}

		return scraperWithStreams(client, streams...)
	}

	t.Run("says why it gave up", func(t *testing.T) {
		t.Parallel()

		scraper := newScraper()

		err := scraper.collectBatch(t.Context(), scraper.enhancedStreams(time.Now()), newEventSink())

		require.ErrorIs(t, err, errIsolationBudget)
		assert.Equal(t, errorKindOther, errorKind(err), "running out of budget is neither a missing stream nor a refusal")
	})

	t.Run("counts the batch it could not attribute", func(t *testing.T) {
		t.Parallel()

		scraper := newScraper()

		scraper.scrape(t.Context())

		assert.Equal(t, uint64(1), scraper.errorCounts[errorKindOther],
			"a batch left unattributed must be visible as one error, not as silence")
		assert.NotZero(t, scraper.errorCounts[errorKindNotFound],
			"the streams the budget did reach must still be reported")
	})
}

func TestScrapeBatchesStreams(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		streamCount   int
		expectedCalls int
	}{
		{streamCount: 0, expectedCalls: 0},
		{streamCount: 1, expectedCalls: 1},
		{streamCount: 99, expectedCalls: 1},
		{streamCount: 100, expectedCalls: 1},
		{streamCount: 101, expectedCalls: 2},
		{streamCount: 150, expectedCalls: 2},
	} {
		t.Run(fmt.Sprintf("%d streams", testCase.streamCount), func(t *testing.T) {
			t.Parallel()

			client := &fakeLogsClient{events: nil, missing: nil, errs: nil, pageSize: 0, calls: nil}
			scraper := scraperWithStreams(client, resourceIDs(testCase.streamCount)...)

			scraper.scrape(t.Context())

			// An empty LogStreamNames would query every stream of the log group.
			require.Len(t, client.calls, testCase.expectedCalls)

			for _, call := range client.calls {
				assert.NotEmpty(t, call.streams)
				assert.LessOrEqual(t, len(call.streams), maxLogStreamsPerRequest)
			}
		})
	}
}

func TestScrapeStopsOnContextCancellation(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{
		events:   eventsFor(oldResourceID),
		missing:  map[string]struct{}{missingResourceID: {}},
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := scraperWithStreams(client, oldResourceID, missingResourceID)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	metrics, _ := scraper.scrape(ctx)

	assert.Empty(t, metrics)
	assert.LessOrEqual(t, len(client.calls), 1, "a cancelled scrape must not keep calling AWS")
	assert.Zero(t, scraper.missing.len())
}

func TestScrapeIsolatesEachBatchIndependently(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(2 * maxLogStreamsPerRequest)
	healthy := streams[len(streams)-1]

	// Every stream of the first batch is missing, plus the first stream of the second batch.
	missing := make(map[string]struct{}, maxLogStreamsPerRequest+1)
	for _, stream := range streams[:maxLogStreamsPerRequest+1] {
		missing[stream] = struct{}{}
	}

	client := &fakeLogsClient{
		events:   eventsFor(healthy),
		missing:  missing,
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := scraperWithStreams(client, streams...)

	metrics, _ := scraper.scrape(t.Context())

	assert.NotEmpty(t, metrics[testKey(healthy)],
		"a batch full of missing streams must not spend the recovery budget of unrelated batches")
	assert.LessOrEqual(t, len(client.calls), 2*(maxIsolationCalls+1),
		"isolation must stay bounded per batch")
}

func TestScrapeStaggersProbesAcrossScrapes(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(4 * maxProbesPerScrape)
	client := &fakeLogsClient{events: nil, missing: nil, errs: nil, pageSize: 0, calls: nil}
	scraper := scraperWithStreams(client, streams...)

	for _, stream := range streams {
		scraper.missing.mark(stream, time.Now().Add(-2*missingStreamTTL))
	}

	batches := scraper.batches(time.Now())

	require.Len(t, batches, 1)
	assert.Len(t, batches[0], maxProbesPerScrape,
		"a fleet of missing streams must not spend a whole scrape on probes")
}

func TestScrapeLeavesBatchesForTheNextScrapeWhenTimeRunsOut(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(150)
	healthy := streams[len(streams)-1]
	client := &fakeLogsClient{
		events:   eventsFor(healthy),
		missing:  map[string]struct{}{streams[0]: {}},
		errs:     []error{nil, context.DeadlineExceeded},
		pageSize: 0,
		calls:    nil,
	}
	scraper := scraperWithStreams(client, streams...)
	startTime := scraper.nextStartTime

	metrics, _ := scraper.scrape(t.Context())

	// The scrape is bounded by its interval, so isolation can run out of time. What it did not
	// attribute is retried on the next scrape, with the streams it did exclude already left out.
	for _, call := range client.calls {
		assert.NotContains(t, call.streams, healthy,
			"the batches behind the one that ran out of time wait for the next scrape")
	}

	assert.Empty(t, metrics[testKey(healthy)])
	assert.Equal(t, uint64(1), scraper.errorCounts[errorKindContext])
	assert.Equal(t, startTime, scraper.nextStartTime, "the events left unread must not be skipped")
}

func TestScrapeBoundsIsolationCalls(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(maxLogStreamsPerRequest)

	// The last stream answers, so the rejection is attributed to the streams rather than to the group.
	healthy := streams[len(streams)-1]

	missing := make(map[string]struct{}, len(streams))
	for _, stream := range streams[:len(streams)-1] {
		missing[stream] = struct{}{}
	}

	client := &fakeLogsClient{events: eventsFor(healthy), missing: missing, errs: nil, pageSize: 0, calls: nil}
	scraper := scraperWithStreams(client, streams...)

	scraper.scrape(t.Context())

	assert.LessOrEqual(t, len(client.calls), maxIsolationCalls+1, "isolation must stay bounded within one scrape")

	// Repeated scrapes must converge: every missing stream ends up excluded, so only the stream that
	// answers is left to request.

	for range 50 {
		scraper.scrape(t.Context())
	}

	client.calls = nil

	scraper.scrape(t.Context())

	require.Len(t, client.calls, 1)
	assert.Equal(t, []string{healthy}, client.calls[0].streams)
	assert.Equal(t, len(missing), scraper.missing.len())
}
