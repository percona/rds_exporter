package enhanced

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// groupMissingClient returns a client that rejects every request the way CloudWatch does when the
// log group does not exist: no stream of the group can be found, whichever ones are asked for.
func groupMissingClient(streams ...string) *fakeLogsClient {
	missing := make(map[string]struct{}, len(streams))
	for _, stream := range streams {
		missing[stream] = struct{}{}
	}

	return &fakeLogsClient{events: nil, missing: missing, errs: nil, pageSize: 0, calls: nil}
}

// blamedGroupScraper returns a session that collected once and then had every one of its log
// streams disappear at the same time, which is the evidence that makes it blame the log group and
// the history that makes it worth giving up on the pause.
func blamedGroupScraper(t *testing.T, streams ...string) (*scraper, *fakeLogsClient) {
	t.Helper()

	client := &fakeLogsClient{
		events:   eventsFor(streams...),
		missing:  nil,
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := scraperWithStreams(client, streams...)

	scraper.scrape(t.Context())

	client.events = nil
	client.missing = make(map[string]struct{}, len(streams))

	for _, stream := range streams {
		client.missing[stream] = struct{}{}
	}

	scraper.scrape(t.Context())

	require.False(t, scraper.groupProbeAfter.IsZero(), "the group must be the one taking the blame")

	return scraper, client
}

// probedStreams runs the given number of scrapes with the log group probe always due, and returns
// the stream each of them probed. A scrape that gave up on probing opens with the whole fleet
// instead of one stream, and contributes nothing.
func probedStreams(t *testing.T, scraper *scraper, client *fakeLogsClient, scrapes int) []string {
	t.Helper()

	probed := make([]string, 0, scrapes)

	for range scrapes {
		scraper.groupProbeAfter = time.Now().Add(-time.Minute)
		client.calls = nil

		scraper.scrape(t.Context())

		require.NotEmpty(t, client.calls, "a due probe must be requested")

		first := client.calls[0].streams
		if len(first) == 1 {
			probed = append(probed, first[0])
		}
	}

	return probed
}

// fallbackGaps runs the given number of scrapes with the log group probe always due, and returns
// how many probes each fallback to isolating log streams waited for. A fallback opens with the
// whole fleet rather than with one stream, which is what tells it apart from a probe.
func fallbackGaps(t *testing.T, scraper *scraper, client *fakeLogsClient, scrapes int) []int {
	t.Helper()

	gaps := make([]int, 0, scrapes)
	probes := 0

	for range scrapes {
		scraper.groupProbeAfter = time.Now().Add(-time.Minute)
		client.calls = nil

		scraper.scrape(t.Context())

		require.NotEmpty(t, client.calls, "a due probe must be requested")

		if len(client.calls[0].streams) == 1 {
			probes++

			continue
		}

		gaps = append(gaps, probes)
		probes = 0
	}

	return gaps
}

func TestScrapeBlamesTheLogGroupWhenNothingAnswers(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(4)
	client := groupMissingClient(streams...)
	scraper := scraperWithStreams(client, streams...)

	scraper.scrape(t.Context())

	assert.Zero(t, scraper.missing.len(), "a stream that exists must not be excluded for the group's sake")
	assert.Equal(t, uint64(1), scraper.errorCounts[errorKindGroupNotFound],
		"the group is one problem, not one per instance")
	assert.Zero(t, scraper.errorCounts[errorKindNotFound])
	assert.False(t, scraper.groupProbeAfter.IsZero(), "the group must be retried")
}

func TestScrapeStopsRequestingWhileTheLogGroupIsMissing(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(4)
	client := groupMissingClient(streams...)
	scraper := scraperWithStreams(client, streams...)

	scraper.scrape(t.Context())

	client.calls = nil
	scraper.errorCounts = make(map[string]uint64)

	scraper.scrape(t.Context())

	assert.Empty(t, client.calls, "a group that cannot exist yet must not be asked once per instance")
	assert.Zero(t, scraper.errorCounts[errorKindGroupNotFound],
		"an already reported group must not inflate the counter on every scrape")
}

func TestScrapeProbesTheLogGroupWithOneStream(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(4)
	client := groupMissingClient(streams...)
	scraper := scraperWithStreams(client, streams...)

	scraper.scrape(t.Context())

	scraper.groupProbeAfter = time.Now().Add(-time.Minute)
	client.calls = nil

	scraper.scrape(t.Context())

	require.Len(t, client.calls, 1, "one stream answers for the whole group")
	assert.Equal(t, streams[:1], client.calls[0].streams)
	assert.Zero(t, scraper.missing.len(), "a probe rejected for the group says nothing about its stream")
	assert.True(t, scraper.groupProbeAfter.After(time.Now()), "a failed probe must wait another TTL")
}

func TestScrapeRecoversWhenTheLogGroupExistsAgain(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(4)
	client := groupMissingClient(streams...)
	scraper := scraperWithStreams(client, streams...)

	scraper.scrape(t.Context())

	client.missing = map[string]struct{}{}
	client.events = eventsFor(streams...)
	scraper.groupProbeAfter = time.Now().Add(-time.Minute)

	metrics, _ := scraper.scrape(t.Context())
	require.Empty(t, metrics[testKey(streams[1])], "the probe only asks for one stream")
	assert.True(t, scraper.groupProbeAfter.IsZero(), "an answered request is all the evidence the group exists")

	client.calls = nil

	metrics, _ = scraper.scrape(t.Context())

	require.Len(t, client.calls, 1)
	assert.Equal(t, streams, client.calls[0].streams, "the whole fleet is requested again")

	for _, stream := range streams {
		assert.NotEmpty(t, metrics[testKey(stream)])
	}
}

func TestScrapeDoesNotBlameTheLogGroupWithoutEvidence(t *testing.T) {
	t.Parallel()

	t.Run("a batch of one stream", func(t *testing.T) {
		t.Parallel()

		client := groupMissingClient(missingResourceID)
		scraper := scraperWithStreams(client, missingResourceID)

		scraper.scrape(t.Context())

		assert.Equal(t, 1, scraper.missing.len(), "one stream cannot tell itself apart from its group")
		assert.Equal(t, uint64(1), scraper.errorCounts[errorKindNotFound])
		assert.Zero(t, scraper.errorCounts[errorKindGroupNotFound])
	})

	t.Run("one batch of several", func(t *testing.T) {
		t.Parallel()

		// A missing group would have rejected the other batch too, so a batch that is merely gone
		// says nothing about it.
		streams := resourceIDs(maxLogStreamsPerRequest + 1)
		client := groupMissingClient(streams[:maxLogStreamsPerRequest]...)
		client.events = eventsFor(streams[maxLogStreamsPerRequest])
		scraper := scraperWithStreams(client, streams...)

		metrics, _ := scraper.scrape(t.Context())

		assert.Zero(t, scraper.errorCounts[errorKindGroupNotFound])
		assert.Equal(t, maxLogStreamsPerRequest, scraper.missing.len())
		assert.NotEmpty(t, metrics[testKey(streams[maxLogStreamsPerRequest])],
			"the batch that answered must keep reporting")
	})

	t.Run("a session of two streams", func(t *testing.T) {
		t.Parallel()

		// A pair of instances leaving CloudWatch together is fleet churn. Only a session monitoring
		// nothing besides that pair could read it as the group, and it would pause itself to do so.
		streams := resourceIDs(2)
		client := groupMissingClient(streams...)
		scraper := scraperWithStreams(client, streams...)

		scraper.scrape(t.Context())

		assert.Zero(t, scraper.errorCounts[errorKindGroupNotFound])
		assert.Equal(t, uint64(len(streams)), scraper.errorCounts[errorKindNotFound])
		assert.Equal(t, len(streams), scraper.missing.len())
		assert.True(t, scraper.groupProbeAfter.IsZero(), "churn must not pause the whole session")
	})

	t.Run("a half that answered", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(4)
		client := groupMissingClient(streams[:len(streams)-1]...)
		client.events = eventsFor(streams[len(streams)-1])
		scraper := scraperWithStreams(client, streams...)

		scraper.scrape(t.Context())

		assert.Equal(t, len(streams)-1, scraper.missing.len())
		assert.Zero(t, scraper.errorCounts[errorKindGroupNotFound])
	})
}

func TestScrapeDoesNotBlameTheLogGroupForAThrottledScrape(t *testing.T) {
	t.Parallel()

	// The throttled batch was not heard from, and what it would have said is exactly what tells a
	// missing group from a few missing streams. Reading the rejections without it would take every
	// healthy instance down for a TTL over the most ordinary thing AWS does.
	streams := resourceIDs(maxLogStreamsPerRequest + minStreamsToBlameTheGroup)
	healthy, gone := streams[:maxLogStreamsPerRequest], streams[maxLogStreamsPerRequest:]
	client := groupMissingClient(gone...)
	client.events = eventsFor(healthy...)
	client.errs = []error{throttlingError()}
	scraper := scraperWithStreams(client, streams...)

	scraper.scrape(t.Context())

	assert.Zero(t, scraper.errorCounts[errorKindGroupNotFound])
	assert.True(t, scraper.groupProbeAfter.IsZero(), "a throttled scrape may not pause the whole session")
	assert.Equal(t, len(gone), scraper.missing.len(), "the streams singled out are missing either way")

	metrics, _ := scraper.scrape(t.Context())

	assert.Len(t, metrics, len(healthy), "the batch that was throttled must report on the next scrape")
}

func TestScrapeBlamesTheLogGroupAcrossEveryBatch(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		streams int
	}{
		{name: "a full batch", streams: maxLogStreamsPerRequest},
		{name: "more streams than one batch holds", streams: 2*maxLogStreamsPerRequest + 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			streams := resourceIDs(testCase.streams)
			client := groupMissingClient(streams...)
			scraper := scraperWithStreams(client, streams...)

			scraper.scrape(t.Context())

			assert.Equal(t, uint64(1), scraper.errorCounts[errorKindGroupNotFound],
				"a scrape nothing answered anywhere is about the group however many requests it took")
			assert.Zero(t, scraper.errorCounts[errorKindNotFound],
				"no instance may be named for a fault that belongs to the group")
			assert.Zero(t, scraper.missing.len())
			assert.True(t, scraper.groupProbeAfter.After(time.Now()))
		})
	}
}

func TestScrapeRotatesTheLogGroupProbe(t *testing.T) {
	t.Parallel()

	t.Run("asks a different stream every time", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(4)
		scraper, client := blamedGroupScraper(t, streams...)

		probed := probedStreams(t, scraper, client, 2*len(streams))

		assert.Equal(t,
			[]string{streams[0], streams[1], streams[2], streams[3], streams[0], streams[1], streams[2]},
			probed,
			"every stream gets a turn, the rotation carries on across a fallback, and only one fallback "+
				"fits in these scrapes because the second waits longer than the first")
	})

	t.Run("recovers when the stream it probed first is the missing one", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(4)
		client := groupMissingClient(streams...)
		scraper := scraperWithStreams(client, streams...)

		scraper.scrape(t.Context())

		// The group is back, and the stream the first probe names is the only thing still gone.
		client.missing = map[string]struct{}{streams[0]: {}}
		client.events = eventsFor(streams[1:]...)

		var metrics map[instanceKey]instanceMetrics

		for range len(streams) + 1 {
			if !scraper.groupProbeAfter.IsZero() {
				scraper.groupProbeAfter = time.Now().Add(-time.Minute)
			}

			metrics, _ = scraper.scrape(t.Context())
		}

		assert.True(t, scraper.groupProbeAfter.IsZero(), "a probe that was answered must end the pause")
		assert.True(t, scraper.missing.marked(streams[0]),
			"the stream that is really gone must be excluded once the group stops taking the blame")

		for _, stream := range streams[1:] {
			assert.NotEmpty(t, metrics[testKey(stream)], "an instance whose stream exists must report again")
		}
	})
}

func TestScrapeIsolatesTheStreamsItsProbesKeptLandingOn(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(10)
	dead, alive := streams[:len(streams)-1], streams[len(streams)-1]
	scraper, client := blamedGroupScraper(t, streams...)

	// The group is back, and with it one stream that the rotation would not reach for another eight
	// probes -- each of which would buy the pause holding that instance back another TTL.
	client.missing = make(map[string]struct{}, len(dead))

	for _, stream := range dead {
		client.missing[stream] = struct{}{}
	}

	client.events = eventsFor(alive)

	var metrics map[instanceKey]instanceMetrics

	for range maxRejectedProbes + 1 {
		scraper.groupProbeAfter = time.Now().Add(-time.Minute)

		metrics, _ = scraper.scrape(t.Context())
	}

	assert.True(t, scraper.groupProbeAfter.IsZero(), "a pause no probe answers must not outlive them")
	assert.Equal(t, len(dead), scraper.missing.len(), "the streams the probes landed on must be excluded")
	assert.NotEmpty(t, metrics[testKey(alive)], "the instance whose stream exists must report again")
}

func TestScrapeKeepsProbingALogGroupThatNeverAnswered(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(4)
	client := groupMissingClient(streams...)
	scraper := scraperWithStreams(client, streams...)

	scraper.scrape(t.Context())

	probed := probedStreams(t, scraper, client, 2*maxRejectedProbes)

	assert.Len(t, probed, 2*maxRejectedProbes,
		"a region that has never published Enhanced Monitoring must not be bisected for it")
	assert.False(t, scraper.groupProbeAfter.IsZero(), "the group stays paused while its probes are rejected")
	assert.Zero(t, scraper.missing.len(), "a probe rejected for the group still says nothing about its stream")
}

func TestScrapeReportsALogGroupOutageOnce(t *testing.T) {
	t.Parallel()

	scraper, client := blamedGroupScraper(t, resourceIDs(10)...)

	gaps := fallbackGaps(t, scraper, client, 32)
	require.NotEmpty(t, gaps, "the fallbacks this is about must have happened")

	assert.Equal(t, uint64(1), scraper.errorCounts[errorKindGroupNotFound],
		"one group that went missing once is one error, however many fallbacks went looking for it")
	assert.Zero(t, scraper.missing.len(),
		"no instance may be named for a fault that belongs to the group")
	assert.False(t, scraper.groupProbeAfter.IsZero(), "the group is still the one taking the blame")
}

func TestScrapeStopsPayingForFallbacksThatFindNothing(t *testing.T) {
	t.Parallel()

	scraper, client := blamedGroupScraper(t, resourceIDs(10)...)

	gaps := fallbackGaps(t, scraper, client, 32)

	require.GreaterOrEqual(t, len(gaps), 3, "three fallbacks must fit in these scrapes")
	assert.Equal(t, []int{maxRejectedProbes, 2 * maxRejectedProbes, 4 * maxRejectedProbes}, gaps[:3],
		"a fallback that found nothing makes the next one wait twice as long")
}

func TestScrapeDoesNotCountAThrottledLogGroupProbe(t *testing.T) {
	t.Parallel()

	scraper, client := blamedGroupScraper(t, resourceIDs(10)...)

	for range maxRejectedProbes + 1 {
		scraper.groupProbeAfter = time.Now().Add(-time.Minute)
		client.errs = []error{throttlingError()}
		client.calls = nil

		scraper.scrape(t.Context())

		require.Len(t, client.calls, 1, "a due probe is one request, throttled or not")
		assert.Len(t, client.calls[0].streams, 1,
			"rate limiting alone must not talk the session into bisecting the whole fleet")
	}

	assert.Zero(t, scraper.rejectedProbes, "only a rejection counts against the group")
	assert.True(t, scraper.groupProbeAfter.After(time.Now()),
		"a throttled probe has spent its turn, so the pause backs off rather than repeating every scrape")
}
