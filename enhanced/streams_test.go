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

func TestScrapeReprobesMissingStream(t *testing.T) {
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

func TestScrapeBoundsIsolationCalls(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(maxLogStreamsPerRequest)

	missing := make(map[string]struct{}, len(streams))
	for _, stream := range streams {
		missing[stream] = struct{}{}
	}

	client := &fakeLogsClient{events: nil, missing: missing, errs: nil, pageSize: 0, calls: nil}
	scraper := scraperWithStreams(client, streams...)

	scraper.scrape(t.Context())

	assert.LessOrEqual(t, len(client.calls), maxIsolationCalls+1, "isolation must stay bounded within one scrape")

	// Repeated scrapes must converge: every stream ends up excluded, so no request is left to make.

	for range 50 {
		scraper.scrape(t.Context())
	}

	client.calls = nil

	scraper.scrape(t.Context())

	assert.Empty(t, client.calls)
	assert.Equal(t, len(streams), scraper.missing.len())
}
