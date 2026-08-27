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

func TestScrapeExportsEventsTimestampedInTheFuture(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		skew    time.Duration
		skewed  bool
		follows bool
	}{
		{name: "behind the exporter's clock", skew: -30 * time.Second, skewed: false, follows: true},
		{name: "within the reported clock skew", skew: clockSkewReportThreshold / 2, skewed: false, follows: false},
		{name: "at the reported clock skew", skew: clockSkewReportThreshold, skewed: false, follows: false},
		{name: "far beyond the reported clock skew", skew: 90 * time.Minute, skewed: true, follows: false},
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

			metrics, _ := scraper.scrape(t.Context())

			// However wrong the two clocks are about each other, the sample is what the instance is
			// judged by, so no skew may cost it.
			assert.NotEmpty(t, metrics[testKey(oldResourceID)])

			skewed := uint64(0)
			if testCase.skewed {
				skewed = 1
			}

			assert.Equal(t, skewed, scraper.skewedEvents)

			if testCase.follows {
				assert.Equal(t, eventTime, scraper.nextStartTime,
					"an event the exporter's own clock explains decides the next window unchanged")

				return
			}

			assert.False(t, scraper.nextStartTime.After(time.Now()),
				"a future timestamp must not push the request window past events still to arrive")
		})
	}
}

func TestScrapeKeepsReportingThroughOneFutureDatedEvent(t *testing.T) {
	t.Parallel()

	happened := testEventTime()
	glitched := time.Now().Add(90 * time.Minute).UTC().Truncate(time.Second)
	client := &fakeLogsClient{
		events: map[string][]types.FilteredLogEvent{
			oldResourceID: {osMetricsEvent(oldResourceID, happened), osMetricsEvent(oldResourceID, glitched)},
		},
		missing:  nil,
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := scraperWithStreams(client, oldResourceID)

	metrics, _ := scraper.scrape(t.Context())

	// The collector judges an instance by the raw timestamp, so a glitched event that wins here sits
	// ahead of every real one until now catches up, and the instance goes dark meanwhile.
	assert.Equal(t, happened, metrics[testKey(oldResourceID)].eventTime,
		"an instance is judged by its newest event that has actually happened")
	assert.Equal(t, happened, scraper.nextStartTime,
		"the window must keep following the events the instance really published")
	assert.Equal(t, uint64(1), scraper.skewedEvents,
		"the glitch is still worth reporting, it just decides nothing")
}

func TestAdvanceStartTimeNeverLeavesTheWindow(t *testing.T) {
	t.Parallel()

	// The clamps hold for any skew, which is what keeps a wrong clock from costing visibility rather
	// than accuracy. Neither bound is a tolerance to be tuned.
	for _, testCase := range []struct {
		name         string
		oldestNewest time.Time
	}{
		{name: "an event dated far in the future", oldestNewest: time.Now().Add(90 * time.Minute)},
		{name: "an event dated before the lookback", oldestNewest: time.Now().Add(-2 * time.Hour)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			scraper := scraperWithStreams(nil, oldResourceID)

			scraper.advanceStartTime(testCase.oldestNewest, true)

			now := time.Now()

			assert.False(t, scraper.nextStartTime.After(now))
			assert.False(t, scraper.nextStartTime.Before(now.Add(-maxLookback-time.Second)))
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
