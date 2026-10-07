package enhanced

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/percona/rds_exporter/sessions"
)

const renamedInstance = blueGreenPrimaryInstance + "-old1"

// eventsClient returns a client whose streams deliver the given events.
func eventsClient(events map[string][]types.FilteredLogEvent) *fakeLogsClient {
	return &fakeLogsClient{events: events, missing: nil, errs: nil, pageSize: 0, calls: nil}
}

// payloadInstancePattern matches the payload_instance value of a logfmt line, quoted or bare.
var payloadInstancePattern = regexp.MustCompile(`payload_instance=("(?:[^"\\]|\\.)*"|\S*)`)

// loggedPayloadInstances returns every payload_instance value logged into buf, unquoted.
func loggedPayloadInstances(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()

	matches := payloadInstancePattern.FindAllStringSubmatch(buf.String(), -1)
	values := make([]string, 0, len(matches))

	for _, match := range matches {
		value := match[1]
		if strings.HasPrefix(value, `"`) {
			unquoted, err := strconv.Unquote(value)
			require.NoError(t, err, "logfmt quotes a value the way Go does")

			value = unquoted
		}

		values = append(values, value)
	}

	return values
}

func TestForeignEvents(t *testing.T) {
	t.Parallel()

	t.Run("stores nothing for an event naming a renamed box", func(t *testing.T) {
		t.Parallel()

		client := eventsClient(map[string][]types.FilteredLogEvent{
			oldResourceID: {osMetricsEventFor(oldResourceID, renamedInstance, testEventTime())},
		})
		scraper := newTestScraperWithClient(client, []sessions.Instance{
			testInstance(blueGreenPrimaryInstance, oldResourceID),
		})

		metrics := scraper.scrape(t.Context())

		assert.Empty(t, metrics, "the box AWS renamed is not the instance PMM was configured with")
		assert.Equal(t, uint64(1), scraper.foreignEvents)
	})

	t.Run("keeps the sample of the instance when a newer event names another", func(t *testing.T) {
		t.Parallel()

		own := testEventTime().Add(-time.Minute)
		client := eventsClient(map[string][]types.FilteredLogEvent{
			oldResourceID: {
				osMetricsEventFor(oldResourceID, blueGreenPrimaryInstance, own),
				osMetricsEventFor(oldResourceID, renamedInstance, testEventTime()),
			},
		})
		scraper := newTestScraperWithClient(client, []sessions.Instance{
			testInstance(blueGreenPrimaryInstance, oldResourceID),
		})

		metrics := scraper.scrape(t.Context())

		require.Contains(t, metrics, testKey(blueGreenPrimaryInstance))
		assert.Equal(t, own, metrics[testKey(blueGreenPrimaryInstance)].eventTime,
			"a foreign event may not supersede the instance's own")
		assert.Equal(t, uint64(1), scraper.foreignEvents)
	})

	t.Run("drops an event whose document names no instance", func(t *testing.T) {
		t.Parallel()

		client := eventsClient(map[string][]types.FilteredLogEvent{
			oldResourceID: {osMetricsEventFor(oldResourceID, "", testEventTime())},
		})
		scraper := newTestScraperWithClient(client, []sessions.Instance{
			testInstance(blueGreenPrimaryInstance, oldResourceID),
		})

		assert.Empty(t, scraper.scrape(t.Context()))
		assert.Equal(t, uint64(1), scraper.foreignEvents)
	})

	t.Run("compares the names byte for byte", func(t *testing.T) {
		t.Parallel()

		client := eventsClient(map[string][]types.FilteredLogEvent{
			oldResourceID: {osMetricsEventFor(oldResourceID, strings.ToUpper(blueGreenPrimaryInstance), testEventTime())},
		})
		scraper := newTestScraperWithClient(client, []sessions.Instance{
			testInstance(blueGreenPrimaryInstance, oldResourceID),
		})

		assert.Empty(t, scraper.scrape(t.Context()),
			"the configured name is the exact key DescribeDBInstances is read by, so the event must match it exactly")
	})

	t.Run("checks every instance sharing the stream on its own", func(t *testing.T) {
		t.Parallel()

		client := eventsClient(map[string][]types.FilteredLogEvent{
			sameResourceID: {osMetricsEventFor(sameResourceID, unchangedPrimaryInstance, testEventTime())},
		})
		scraper := newTestScraperWithClient(client, []sessions.Instance{
			testInstance(blueGreenPrimaryInstance, sameResourceID),
			testInstance(unchangedPrimaryInstance, sameResourceID),
		})

		metrics := scraper.scrape(t.Context())

		assert.NotContains(t, metrics, testKey(blueGreenPrimaryInstance))
		assert.Contains(t, metrics, testKey(unchangedPrimaryInstance))
		assert.Equal(t, uint64(1), scraper.foreignEvents)
	})

	t.Run("counts the request as answered", func(t *testing.T) {
		t.Parallel()

		client := eventsClient(map[string][]types.FilteredLogEvent{
			oldResourceID: {osMetricsEventFor(oldResourceID, renamedInstance, testEventTime())},
		})
		scraper := newTestScraperWithClient(client, []sessions.Instance{
			testInstance(blueGreenPrimaryInstance, oldResourceID),
		})

		scraper.scrape(t.Context())

		assert.True(t, scraper.evidence.answered)
		assert.Zero(t, scraper.missing.len(), "a stream that answered is not missing, whoever its events name")
		assert.False(t, scraper.group.blamed)
		assert.Empty(t, scraper.errorCounts, "a foreign event is not a collection error")
	})

	t.Run("does not move the request window", func(t *testing.T) {
		t.Parallel()

		client := eventsClient(map[string][]types.FilteredLogEvent{
			oldResourceID: {osMetricsEventFor(oldResourceID, renamedInstance, testEventTime())},
		})
		scraper := newTestScraperWithClient(client, []sessions.Instance{
			testInstance(blueGreenPrimaryInstance, oldResourceID),
		})
		start := scraper.nextStartTime

		scraper.scrape(t.Context())

		assert.Equal(t, start, scraper.nextStartTime,
			"the instance's own events from before the foreign one must still be requested")
	})

	t.Run("does not extend the stored sample", func(t *testing.T) {
		t.Parallel()

		eventTime := testEventTime()
		client := eventsClient(map[string][]types.FilteredLogEvent{
			oldResourceID: {osMetricsEventFor(oldResourceID, blueGreenPrimaryInstance, eventTime.Add(-time.Minute))},
		})
		scraper := newTestScraperWithClient(client, []sessions.Instance{
			testInstance(blueGreenPrimaryInstance, oldResourceID),
		})
		collector := configuredCollector(map[instanceKey]storedSample{}, blueGreenPrimaryInstance)

		now := time.Now()
		collector.setMetrics(scraper.result(scraper.scrape(t.Context())), now)
		expiresAt := collector.metrics[testKey(blueGreenPrimaryInstance)].expiresAt

		client.events[oldResourceID] = []types.FilteredLogEvent{osMetricsEventFor(oldResourceID, renamedInstance, eventTime)}
		foreignOnly := scraper.result(scraper.scrape(t.Context()))
		collector.setMetrics(foreignOnly, now.Add(time.Minute))

		assert.Equal(t, expiresAt, collector.metrics[testKey(blueGreenPrimaryInstance)].expiresAt)

		up := findMetric(collectSamplesAt(t, collector, expiresAt), upMetricName, blueGreenPrimaryInstance)
		require.NotNil(t, up)
		assert.InDelta(t, 0.0, up.Value, 0, "with only foreign events the sample expires on schedule")
	})

	t.Run("never exports the name the document carries", func(t *testing.T) {
		t.Parallel()

		client := eventsClient(map[string][]types.FilteredLogEvent{
			oldResourceID: {osMetricsEventFor(oldResourceID, renamedInstance, testEventTime())},
			sameResourceID: {
				osMetricsEventFor(sameResourceID, unchangedPrimaryInstance, testEventTime()),
			},
		})
		scraper := newTestScraperWithClient(client, []sessions.Instance{
			testInstance(blueGreenPrimaryInstance, oldResourceID),
			testInstance(unchangedPrimaryInstance, sameResourceID),
		})
		collector := configuredCollector(map[instanceKey]storedSample{}, blueGreenPrimaryInstance, unchangedPrimaryInstance)

		collector.setMetrics(scraper.result(scraper.scrape(t.Context())), time.Now())

		metrics := collect(t, collector)
		require.NotEmpty(t, metrics)

		for _, metric := range metrics {
			for name, value := range metric.Labels {
				assert.NotEqual(t, renamedInstance, value, "%s{%s}", metric.Name, name)
			}
		}

		foreign := findMetricByName(metrics, foreignEventsMetricName)
		require.NotNil(t, foreign)
		assert.InDelta(t, 1.0, foreign.Value, 0)
		assert.Equal(t, prometheus.Labels{regionLabel: testRegion}, foreign.Labels)
	})

	t.Run("hands the count to the collector once", func(t *testing.T) {
		t.Parallel()

		client := eventsClient(map[string][]types.FilteredLogEvent{
			oldResourceID: {osMetricsEventFor(oldResourceID, renamedInstance, testEventTime())},
		})
		scraper := newTestScraperWithClient(client, []sessions.Instance{
			testInstance(blueGreenPrimaryInstance, oldResourceID),
		})

		first := scraper.result(scraper.scrape(t.Context()))
		second := scraper.result(map[instanceKey]instanceMetrics{})

		assert.Equal(t, uint64(1), first.foreignEvents)
		assert.Zero(t, second.foreignEvents)
	})

	t.Run("logs a bounded and escaped name", func(t *testing.T) {
		t.Parallel()

		forged := "x\nlevel=error msg=\"forged\" " + strings.Repeat("ü", 200)
		client := eventsClient(map[string][]types.FilteredLogEvent{
			oldResourceID: {osMetricsEventFor(oldResourceID, forged, testEventTime())},
		})

		var buf bytes.Buffer

		scraper := scraperLoggingTo(&buf, client, []sessions.Instance{testInstance(blueGreenPrimaryInstance, oldResourceID)})

		scraper.scrape(t.Context())

		values := loggedPayloadInstances(t, &buf)
		require.NotEmpty(t, values, "the dropped event is logged at debug")

		for _, value := range values {
			assert.True(t, utf8.ValidString(value), "truncation may not split a character")
			assert.LessOrEqual(t, utf8.RuneCountInString(value), maxLoggedPayloadName)
			assert.True(t, strings.HasPrefix(forged, value))
		}

		for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
			assert.False(t, strings.HasPrefix(line, `level=error msg="forged"`),
				"an AWS-controlled document must not be able to inject a log line")
		}
	})
}

func TestTruncate(t *testing.T) {
	t.Parallel()

	const fourLetters = "abcd"

	for _, testCase := range []struct {
		name  string
		value string
		limit int
		want  string
	}{
		{name: "keeps an empty value", value: "", limit: 4, want: ""},
		{name: "keeps a shorter value", value: "abc", limit: 4, want: "abc"},
		{name: "keeps a value at the limit", value: fourLetters, limit: 4, want: fourLetters},
		{name: "cuts a longer value", value: fourLetters + "e", limit: 4, want: fourLetters},
		{name: "counts characters, not bytes", value: "üüüüü", limit: 4, want: "üüüü"},
		{name: "keeps a multibyte value at the limit", value: "aüü€", limit: 4, want: "aüü€"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, testCase.want, truncate(testCase.value, testCase.limit))
		})
	}
}

const (
	refreshingLine = "refreshing the instance states"
	renamedLine    = "renamed in AWS"
)

// tick runs one round of the scraper's loop: the refresh, then the scrape.
func tick(t *testing.T, scraper *scraper) map[instanceKey]instanceMetrics {
	t.Helper()

	scraper.refreshStates(t.Context())

	return scraper.scrape(t.Context())
}

// foreignScraper returns a scraper logging into buf whose regular refresh is parked, so that any
// resolver call is an early one.
func foreignScraper(
	buf *bytes.Buffer, client *fakeLogsClient, resolver *fakeStateResolver, instances ...sessions.Instance,
) *scraper {
	scraper := scraperLoggingTo(buf, client, instances)
	scraper.stateResolver = resolver

	return scraper
}

func renamedBoxClient() *fakeLogsClient {
	return eventsClient(map[string][]types.FilteredLogEvent{
		oldResourceID: {osMetricsEventFor(oldResourceID, renamedInstance, testEventTime())},
	})
}

func TestForeignEventEpisodes(t *testing.T) {
	t.Parallel()

	t.Run("repoints the instance at the next scrape", func(t *testing.T) {
		t.Parallel()

		greenEvent := testEventTime().Add(-time.Minute)
		ownEvent := testEventTime()
		client := eventsClient(map[string][]types.FilteredLogEvent{
			oldResourceID: {osMetricsEventFor(oldResourceID, renamedInstance, ownEvent)},
			newResourceID: {
				// The promoted box reported the name it had before the switchover.
				osMetricsEventFor(newResourceID, blueGreenPrimaryInstance+"-green-abc", greenEvent),
				osMetricsEventFor(newResourceID, blueGreenPrimaryInstance, ownEvent),
			},
		})
		resolver := &fakeStateResolver{
			states: map[string]sessions.InstanceState{blueGreenPrimaryInstance: monitoredState(newResourceID)},
			err:    nil,
			calls:  0,
		}

		var buf bytes.Buffer

		scraper := foreignScraper(&buf, client, resolver, testInstance(blueGreenPrimaryInstance, oldResourceID))
		collector := configuredCollector(map[instanceKey]storedSample{}, blueGreenPrimaryInstance)

		collector.setMetrics(scraper.result(tick(t, scraper)), time.Now())
		require.Zero(t, resolver.calls)

		collector.setMetrics(scraper.result(tick(t, scraper)), time.Now())

		assert.Equal(t, 1, resolver.calls, "the first foreign event makes the refresh due at the next scrape")
		assert.Equal(t, newResourceID, scraper.instances[0].ResourceID)
		assert.Equal(t, ownEvent, collector.metrics[testKey(blueGreenPrimaryInstance)].eventTime)
		assert.Empty(t, scraper.foreign, "the repoint ends the episode")
		assert.Equal(t, 1, strings.Count(buf.String(), refreshingLine))
		assert.NotContains(t, buf.String(), renamedLine)

		for _, metric := range collect(t, collector) {
			if instance, ok := metric.Labels[instanceLabel]; ok {
				assert.Equal(t, blueGreenPrimaryInstance, instance, metric.Name)
			}
		}
	})

	t.Run("warns once when the refresh keeps the resource ID", func(t *testing.T) {
		t.Parallel()

		resolver := &fakeStateResolver{
			states: map[string]sessions.InstanceState{blueGreenPrimaryInstance: monitoredState(oldResourceID)},
			err:    nil,
			calls:  0,
		}

		var buf bytes.Buffer

		scraper := foreignScraper(&buf, renamedBoxClient(), resolver, testInstance(blueGreenPrimaryInstance, oldResourceID))

		for range 4 {
			assert.Empty(t, tick(t, scraper))
		}

		assert.Equal(t, 1, resolver.calls, "a confirmed episode waits for the regular refresh")
		assert.Equal(t, 1, strings.Count(buf.String(), refreshingLine))
		assert.Equal(t, 1, strings.Count(buf.String(), renamedLine))
		assert.Contains(t, buf.String(), "level=warn")
		assert.False(t, scraper.nextResourceIDRefresh.Before(time.Now().Add(resourceIDRefreshInterval-time.Minute)),
			"the regular refresh interval resumes")
	})

	t.Run("does not warn again on the regular refresh", func(t *testing.T) {
		t.Parallel()

		resolver := &fakeStateResolver{
			states: map[string]sessions.InstanceState{blueGreenPrimaryInstance: monitoredState(oldResourceID)},
			err:    nil,
			calls:  0,
		}

		var buf bytes.Buffer

		scraper := foreignScraper(&buf, renamedBoxClient(), resolver, testInstance(blueGreenPrimaryInstance, oldResourceID))

		tick(t, scraper)
		tick(t, scraper)

		scraper.nextResourceIDRefresh = time.Now().Add(-time.Second)

		tick(t, scraper)

		assert.Equal(t, 2, resolver.calls)
		assert.Equal(t, 1, strings.Count(buf.String(), renamedLine), "a confirmed episode is reported once")
	})

	t.Run("starts over when the document names yet another instance", func(t *testing.T) {
		t.Parallel()

		resolver := &fakeStateResolver{
			states: map[string]sessions.InstanceState{blueGreenPrimaryInstance: monitoredState(oldResourceID)},
			err:    nil,
			calls:  0,
		}

		var buf bytes.Buffer

		client := renamedBoxClient()
		scraper := foreignScraper(&buf, client, resolver, testInstance(blueGreenPrimaryInstance, oldResourceID))

		tick(t, scraper)
		tick(t, scraper)
		require.Equal(t, 1, resolver.calls)

		client.events[oldResourceID] = []types.FilteredLogEvent{
			osMetricsEventFor(oldResourceID, blueGreenPrimaryInstance+"-old2", testEventTime()),
		}

		tick(t, scraper)
		tick(t, scraper)
		tick(t, scraper)

		assert.Equal(t, 2, resolver.calls)
		assert.Equal(t, 2, strings.Count(buf.String(), renamedLine))
	})

	t.Run("asks once for every instance a scrape found foreign events for", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(3)
		events := make(map[string][]types.FilteredLogEvent, len(streams))
		instances := make([]sessions.Instance, 0, len(streams))
		states := make(map[string]sessions.InstanceState, len(streams))

		for _, stream := range streams {
			events[stream] = []types.FilteredLogEvent{osMetricsEventFor(stream, stream+"-old1", testEventTime())}
			instances = append(instances, testInstance(stream, stream))
			states[stream] = monitoredState(stream)
		}

		resolver := &fakeStateResolver{states: states, err: nil, calls: 0}

		var buf bytes.Buffer

		scraper := foreignScraper(&buf, eventsClient(events), resolver, instances...)

		tick(t, scraper)
		tick(t, scraper)
		tick(t, scraper)

		assert.Equal(t, 1, resolver.calls)
		assert.Equal(t, len(streams), strings.Count(buf.String(), renamedLine))
	})

	t.Run("keeps the backoff of a failed refresh", func(t *testing.T) {
		t.Parallel()

		resolver := &fakeStateResolver{states: nil, err: errDescribeFailed, calls: 0}

		var buf bytes.Buffer

		client := renamedBoxClient()
		scraper := foreignScraper(&buf, client, resolver, testInstance(blueGreenPrimaryInstance, oldResourceID))

		tick(t, scraper)
		tick(t, scraper)
		require.Equal(t, 1, resolver.calls)
		require.Positive(t, scraper.refreshBackoff)

		// A new episode inside the backoff may not cut it short either.
		client.events[oldResourceID] = []types.FilteredLogEvent{
			osMetricsEventFor(oldResourceID, blueGreenPrimaryInstance+"-old2", testEventTime()),
		}

		tick(t, scraper)
		tick(t, scraper)

		assert.Equal(t, 1, resolver.calls, "a foreign event must not turn a failing refresh into one per scrape")
		assert.NotContains(t, buf.String(), renamedLine, "a failed refresh confirms nothing")

		resolver.err = nil
		resolver.states = map[string]sessions.InstanceState{blueGreenPrimaryInstance: monitoredState(oldResourceID)}
		scraper.nextResourceIDRefresh = time.Now().Add(-time.Second)

		tick(t, scraper)

		assert.Equal(t, 2, resolver.calls)
		assert.Equal(t, 1, strings.Count(buf.String(), renamedLine), "the backed-off refresh settles the episode")
	})

	t.Run("settles what a partial refresh returned", func(t *testing.T) {
		t.Parallel()

		resolver := &fakeStateResolver{
			states: map[string]sessions.InstanceState{blueGreenPrimaryInstance: monitoredState(oldResourceID)},
			err:    errDescribeFailed,
			calls:  0,
		}

		var buf bytes.Buffer

		scraper := foreignScraper(&buf, renamedBoxClient(), resolver, testInstance(blueGreenPrimaryInstance, oldResourceID))

		tick(t, scraper)
		tick(t, scraper)

		assert.Equal(t, 1, resolver.calls)
		assert.Equal(t, 1, strings.Count(buf.String(), renamedLine),
			"a page that was read is authoritative for the instances on it")
	})

	t.Run("does not ask again for an instance the refresh did not return", func(t *testing.T) {
		t.Parallel()

		resolver := &fakeStateResolver{states: map[string]sessions.InstanceState{}, err: nil, calls: 0}

		var buf bytes.Buffer

		scraper := foreignScraper(&buf, renamedBoxClient(), resolver, testInstance(blueGreenPrimaryInstance, oldResourceID))

		for range 4 {
			tick(t, scraper)
		}

		assert.Equal(t, 1, resolver.calls)
		assert.NotContains(t, buf.String(), renamedLine)
	})

	t.Run("does not refresh for an episode the instance's own event ended", func(t *testing.T) {
		t.Parallel()

		resolver := &fakeStateResolver{states: nil, err: nil, calls: 0}
		client := eventsClient(map[string][]types.FilteredLogEvent{
			oldResourceID: {
				osMetricsEventFor(oldResourceID, renamedInstance, testEventTime().Add(-time.Minute)),
				osMetricsEventFor(oldResourceID, blueGreenPrimaryInstance, testEventTime()),
			},
		})

		var buf bytes.Buffer

		scraper := foreignScraper(&buf, client, resolver, testInstance(blueGreenPrimaryInstance, oldResourceID))

		assert.NotEmpty(t, tick(t, scraper))
		tick(t, scraper)

		assert.Zero(t, resolver.calls)
		assert.Empty(t, scraper.foreign)
	})

	t.Run("ends a confirmed episode when the regular refresh repoints the instance", func(t *testing.T) {
		t.Parallel()

		resolver := &fakeStateResolver{
			states: map[string]sessions.InstanceState{blueGreenPrimaryInstance: monitoredState(oldResourceID)},
			err:    nil,
			calls:  0,
		}

		var buf bytes.Buffer

		scraper := foreignScraper(&buf, renamedBoxClient(), resolver, testInstance(blueGreenPrimaryInstance, oldResourceID))

		tick(t, scraper)
		tick(t, scraper)
		require.Len(t, scraper.foreign, 1)

		resolver.states = map[string]sessions.InstanceState{blueGreenPrimaryInstance: monitoredState(newResourceID)}
		scraper.nextResourceIDRefresh = time.Now().Add(-time.Second)

		tick(t, scraper)

		assert.Empty(t, scraper.foreign)
	})
}
