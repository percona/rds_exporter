package enhanced

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func throttlingError() error {
	return &smithy.GenericAPIError{Code: "ThrottlingException", Message: "slow down", Fault: 0}
}

func TestScrapeKeepsStartTimeWhenNoEvents(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{events: nil, missing: nil, errs: nil, pageSize: 0, calls: nil}
	scraper := scraperWithStreams(client, oldResourceID)
	startTime := scraper.nextStartTime

	scraper.scrape(t.Context())

	assert.Equal(t, startTime, scraper.nextStartTime, "an empty scrape must not skip events that arrive late")
}

func TestScrapeAdvancesStartTimeToOldestNewestEvent(t *testing.T) {
	t.Parallel()

	newest := testEventTime()
	older := newest.Add(-10 * time.Second)
	client := &fakeLogsClient{
		events: map[string][]types.FilteredLogEvent{
			oldResourceID:  {osMetricsEvent(oldResourceID, newest)},
			sameResourceID: {osMetricsEvent(sameResourceID, older)},
		},
		missing:  nil,
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := scraperWithStreams(client, oldResourceID, sameResourceID)

	scraper.scrape(t.Context())

	assert.Equal(t, older, scraper.nextStartTime, "the slowest instance decides where the next request starts")
}

func TestScrapeKeepsStartTimeWhenBatchFails(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(150)
	client := &fakeLogsClient{
		events:   eventsFor(streams[0]),
		missing:  nil,
		errs:     []error{nil, throttlingError()},
		pageSize: 0,
		calls:    nil,
	}
	scraper := scraperWithStreams(client, streams...)
	startTime := scraper.nextStartTime

	scraper.scrape(t.Context())

	require.Len(t, client.calls, 2)
	assert.Equal(t, startTime, scraper.nextStartTime, "a partially failed scrape must not drop the events it missed")
}

func TestScrapeIgnoresEventsTimestampedInTheFuture(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name     string
		skew     time.Duration
		accepted bool
	}{
		{name: "within the tolerated clock drift", skew: maxFutureSkew / 2, accepted: true},
		{name: "at the tolerated clock drift", skew: maxFutureSkew, accepted: true},
		{name: "beyond the tolerated clock drift", skew: 90 * time.Minute, accepted: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			eventTime := time.Now().Add(testCase.skew).UTC().Truncate(time.Second)
			client := &fakeLogsClient{
				events: map[string][]types.FilteredLogEvent{
					oldResourceID: {osMetricsEvent(oldResourceID, eventTime)},
				},
				missing:  nil,
				errs:     nil,
				pageSize: 0,
				calls:    nil,
			}
			scraper := scraperWithStreams(client, oldResourceID)
			startTime := scraper.nextStartTime

			metrics, _ := scraper.scrape(t.Context())

			if testCase.accepted {
				assert.NotEmpty(t, metrics[testKey(oldResourceID)])
				assert.Equal(t, eventTime, scraper.nextStartTime)
				assert.Zero(t, scraper.errorCounts[errorKindFutureEvent])

				return
			}

			assert.Empty(t, metrics,
				"a future timestamp would freeze the cache entry it lands in and never expire")
			assert.Equal(t, startTime, scraper.nextStartTime,
				"a future timestamp must not push the request window past legitimate events")
			assert.Equal(t, uint64(1), scraper.errorCounts[errorKindFutureEvent])
		})
	}
}

func TestScrapeClampsStartTimeToLookback(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{events: nil, missing: nil, errs: nil, pageSize: 0, calls: nil}
	scraper := scraperWithStreams(client, oldResourceID)
	scraper.nextStartTime = time.Now().Add(-2 * time.Hour)

	scraper.scrape(t.Context())

	require.Len(t, client.calls, 1)

	earliest := time.Now().Add(-maxLookback).UnixMilli()

	assert.GreaterOrEqual(t, scraper.nextStartTime.UnixMilli(), earliest-time.Second.Milliseconds(),
		"recovering from a long outage must not paginate through hours of events")
}
