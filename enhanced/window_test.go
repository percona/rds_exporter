package enhanced

import (
	"context"
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

func TestScrapeRequestWindow(t *testing.T) {
	t.Parallel()

	t.Run("keeps the start time when nothing answered with an event", func(t *testing.T) {
		t.Parallel()

		client := &fakeLogsClient{events: nil, missing: nil, errs: nil, pageSize: 0, calls: nil}
		scraper := scraperWithStreams(client, oldResourceID)
		startTime := scraper.nextStartTime

		scraper.scrape(t.Context())

		assert.Equal(t, startTime, scraper.nextStartTime, "an empty scrape must not skip events that arrive late")
	})

	t.Run("advances the start time to the oldest of the newest events", func(t *testing.T) {
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
	})

	t.Run("keeps start time when batch fails", func(t *testing.T) {
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
	})

	t.Run("advances start time when time runs out", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(150)
		client := &fakeLogsClient{
			events:   eventsFor(streams[0]),
			missing:  nil,
			errs:     []error{nil, context.DeadlineExceeded},
			pageSize: 0,
			calls:    nil,
		}
		scraper := scraperWithStreams(client, streams...)

		scraper.scrape(t.Context())

		// A window a scrape cannot drain inside one interval would be handed back unchanged, so the next
		// scrape would run out of time on the same events and the session would never report at all.
		require.Len(t, client.calls, 2)
		assert.Equal(t, testEventTime(), scraper.nextStartTime,
			"a scrape that ran out of time must narrow the window to what it did read")
	})

	t.Run("keeps start time when a batch failed before time ran out", func(t *testing.T) {
		t.Parallel()

		// Three batches: the first is throttled, the second answers, the third hits the deadline. The
		// throttled batch is asked again on the next scrape, but only for the events still inside the
		// window, so the deadline must not be allowed to move it past them.
		streams := resourceIDs(2*maxLogStreamsPerRequest + 1)
		client := &fakeLogsClient{
			events:   eventsFor(streams[maxLogStreamsPerRequest]),
			missing:  nil,
			errs:     []error{throttlingError(), nil, context.DeadlineExceeded},
			pageSize: 0,
			calls:    nil,
		}
		scraper := scraperWithStreams(client, streams...)
		startTime := scraper.nextStartTime

		metrics := scraper.scrape(t.Context())

		require.Len(t, client.calls, 3)
		assert.Len(t, metrics, 1, "the batch that answered must still report")
		assert.Equal(t, startTime, scraper.nextStartTime,
			"a deadline must not drop the events of a batch that failed for a reason of its own")
	})

	t.Run("clamps start time to lookback", func(t *testing.T) {
		t.Parallel()

		client := &fakeLogsClient{events: nil, missing: nil, errs: nil, pageSize: 0, calls: nil}
		scraper := scraperWithStreams(client, oldResourceID)
		scraper.nextStartTime = time.Now().Add(-2 * time.Hour)

		scraper.scrape(t.Context())

		require.Len(t, client.calls, 1)

		earliest := time.Now().Add(-maxLookback).UnixMilli()

		assert.GreaterOrEqual(t, scraper.nextStartTime.UnixMilli(), earliest-time.Second.Milliseconds(),
			"recovering from a long outage must not paginate through hours of events")
	})
}

func TestScrapeFutureDatedEvents(t *testing.T) {
	t.Parallel()

	t.Run("exports events timestamped in the future", func(t *testing.T) {
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

				metrics := scraper.scrape(t.Context())

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
	})

	t.Run("follows a host slightly behind AWS", func(t *testing.T) {
		t.Parallel()

		// AWS is ahead of this host by less than the drift worth reporting, so the event an instance has
		// just published is always dated a little ahead of the clock, and the one before it a little behind.
		lead := clockSkewReportThreshold / 2
		previous := time.Now().Add(lead - time.Minute).UTC().Truncate(time.Second)
		latest := previous.Add(time.Minute)
		client := &fakeLogsClient{
			events: map[string][]types.FilteredLogEvent{
				oldResourceID: {osMetricsEvent(oldResourceID, previous)},
			},
			missing:  nil,
			errs:     nil,
			pageSize: 0,
			calls:    nil,
		}
		scraper := scraperWithStreams(client, oldResourceID)

		metrics := scraper.scrape(t.Context())
		require.Equal(t, previous, metrics[testKey(oldResourceID)].eventTime)

		client.events[oldResourceID] = append(client.events[oldResourceID], osMetricsEvent(oldResourceID, latest))

		// The window is clamped to now, which the latest event stays ahead of, so it is delivered again on
		// the scrape after the one that first read it.
		for range 2 {
			metrics = scraper.scrape(t.Context())

			assert.Equal(t, latest, metrics[testKey(oldResourceID)].eventTime,
				"an instance is judged by its latest event, not by the one before it, for as long as the host trails AWS")
			assert.Zero(t, scraper.skewedEvents, "drift within the threshold is not skew")
			assert.False(t, scraper.nextStartTime.After(time.Now()),
				"the window must not run ahead of events still to arrive")
		}
	})

	t.Run("keeps reporting through one future dated event", func(t *testing.T) {
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

		metrics := scraper.scrape(t.Context())

		// A glitched event that wins here becomes both the sample and the window, so the instance would
		// be judged by a document it published before the events that actually describe it.
		assert.Equal(t, happened, metrics[testKey(oldResourceID)].eventTime,
			"an instance is judged by its newest event that has actually happened")
		assert.Equal(t, happened, scraper.nextStartTime,
			"the window must keep following the events the instance really published")
		assert.Equal(t, uint64(1), scraper.skewedEvents,
			"the glitch is still worth reporting, it just decides nothing")
	})

	t.Run("keeps reporting after a future dated event", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		glitched := now.Add(90 * time.Minute).UTC().Truncate(time.Second)
		client := &fakeLogsClient{
			events: map[string][]types.FilteredLogEvent{
				oldResourceID: {osMetricsEvent(oldResourceID, glitched)},
			},
			missing:  nil,
			errs:     nil,
			pageSize: 0,
			calls:    nil,
		}
		scraper := scraperWithStreams(client, oldResourceID)
		collector := configuredCollector(map[instanceKey]storedSample{}, oldResourceID)

		// A scrape with nothing but a future dated event to go by, which is what an exporter whose clock
		// is behind AWS collects.
		metrics := scraper.scrape(t.Context())
		collector.setMetrics(futureDatedResult(metrics), now)

		// The instance then publishes an event the exporter's own clock agrees with.
		happened := time.Now().UTC()
		client.events[oldResourceID] = append(client.events[oldResourceID], osMetricsEvent(oldResourceID, happened))

		metrics = scraper.scrape(t.Context())
		collector.setMetrics(futureDatedResult(metrics), happened)

		samples := collectSamplesAt(t, collector, happened.Add(2*time.Minute))

		require.NotNil(t, findMetric(samples, lastEventMetricName, oldResourceID))
		assert.InDelta(t, float64(happened.Unix()), findMetric(samples, lastEventMetricName, oldResourceID).Value, 0,
			"one future dated event must not suppress the events behind it")
		assert.NotNil(t, findMetric(samples, osMetricName, oldResourceID))
		require.NotNil(t, findMetric(samples, upMetricName, oldResourceID))
		assert.InDelta(t, 1.0, findMetric(samples, upMetricName, oldResourceID).Value, 0)
	})
}

// futureDatedResult packages a scrape of oldResourceID for the collector.
func futureDatedResult(metrics map[instanceKey]instanceMetrics) scrapeResult {
	return scrapeResult{
		metrics:      metrics,
		errorCounts:  nil,
		skewedEvents: 0,
		monitored:    map[instanceKey]bool{testKey(oldResourceID): true},
		region:       testRegion,
		interval:     time.Minute,
	}
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
