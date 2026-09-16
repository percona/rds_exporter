package enhanced

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// excludedLogStream is the log line a stream is excluded with, whatever level it is logged at.
const excludedLogStream = `msg="CloudWatch log stream does not exist; excluding it from Enhanced Monitoring requests."`

// groupMissingClient returns a client that rejects every request the way CloudWatch does when the
// log group does not exist: no stream of the group can be found, whichever ones are asked for.
func groupMissingClient(streams ...string) *fakeLogsClient {
	missing := missingSet(streams...)

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
	client.missing = missingSet(streams...)

	scraper.scrape(t.Context())

	require.False(t, scraper.group.probeAfter.IsZero(), "the group must be the one taking the blame")

	return scraper, client
}

// probedStreams runs the given number of scrapes with the log group probe always due, and returns
// the stream each of them probed. A scrape that gave up on probing opens with the whole fleet
// instead of one stream, and contributes nothing.
func probedStreams(t *testing.T, scraper *scraper, client *fakeLogsClient, scrapes int) []string {
	t.Helper()

	probed := make([]string, 0, scrapes)

	for range scrapes {
		scraper.group.probeAfter = time.Now().Add(-time.Minute)
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
		scraper.group.probeAfter = time.Now().Add(-time.Minute)
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
	assert.False(t, scraper.group.probeAfter.IsZero(), "the group must be retried")
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

	scraper.group.probeAfter = time.Now().Add(-time.Minute)
	client.calls = nil

	scraper.scrape(t.Context())

	require.Len(t, client.calls, 1, "one stream answers for the whole group")
	assert.Equal(t, streams[:1], client.calls[0].streams)
	assert.Zero(t, scraper.missing.len(), "a probe rejected for the group says nothing about its stream")
	assert.True(t, scraper.group.probeAfter.After(time.Now()), "a failed probe must wait another TTL")
}

func TestScrapeRecoversWhenTheLogGroupExistsAgain(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(4)
	client := groupMissingClient(streams...)
	scraper := scraperWithStreams(client, streams...)

	scraper.scrape(t.Context())

	client.missing = map[string]struct{}{}
	client.events = eventsFor(streams...)
	scraper.group.probeAfter = time.Now().Add(-time.Minute)

	metrics, _ := scraper.scrape(t.Context())
	require.Empty(t, metrics[testKey(streams[1])], "the probe only asks for one stream")
	assert.True(t, scraper.group.probeAfter.IsZero(), "an answered request is all the evidence the group exists")

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
		assert.True(t, scraper.group.probeAfter.IsZero(), "churn must not pause the whole session")
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
	assert.True(t, scraper.group.probeAfter.IsZero(), "a throttled scrape may not pause the whole session")
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
			assert.True(t, scraper.group.probeAfter.After(time.Now()))
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
			if !scraper.group.probeAfter.IsZero() {
				scraper.group.probeAfter = time.Now().Add(-time.Minute)
			}

			metrics, _ = scraper.scrape(t.Context())
		}

		assert.True(t, scraper.group.probeAfter.IsZero(), "a probe that was answered must end the pause")
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
	client.missing = missingSet(dead...)

	client.events = eventsFor(alive)

	var metrics map[instanceKey]instanceMetrics

	for range maxRejectedProbes + 1 {
		scraper.group.probeAfter = time.Now().Add(-time.Minute)

		metrics, _ = scraper.scrape(t.Context())
	}

	assert.True(t, scraper.group.probeAfter.IsZero(), "a pause no probe answers must not outlive them")
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
	assert.False(t, scraper.group.probeAfter.IsZero(), "the group stays paused while its probes are rejected")
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
	assert.False(t, scraper.group.probeAfter.IsZero(), "the group is still the one taking the blame")
}

func TestScrapeStopsPayingForFallbacksThatFindNothing(t *testing.T) {
	t.Parallel()

	scraper, client := blamedGroupScraper(t, resourceIDs(10)...)

	gaps := fallbackGaps(t, scraper, client, 32)

	require.GreaterOrEqual(t, len(gaps), 3, "three fallbacks must fit in these scrapes")
	assert.Equal(t, []int{maxRejectedProbes, 2 * maxRejectedProbes, 4 * maxRejectedProbes}, gaps[:3],
		"a fallback that found nothing makes the next one wait twice as long")
}

func TestScrapeWarnsAboutAMissingLogStreamOnlyOnceTheLogGroupAnswers(t *testing.T) {
	t.Parallel()

	// fallingBack returns a blamed session about to give its pause up for a bisect, logging into buf.
	fallingBack := func(t *testing.T, buf *bytes.Buffer, streams ...string) (*scraper, *fakeLogsClient) {
		t.Helper()

		scraper, client := blamedGroupScraper(t, streams...)
		scraper.logger = level.NewFilter(log.NewLogfmtLogger(buf), level.AllowDebug())
		scraper.group.rejectedProbes = maxRejectedProbes
		scraper.group.probeAfter = time.Now().Add(-time.Minute)

		return scraper, client
	}

	t.Run("does not warn while the group is the suspect", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		// The deadline cuts the bisect after four rejections, so the streams it singled out are excluded
		// without the group ever being able to take the blame for them.
		scraper, client := fallingBack(t, &buf, resourceIDs(10)...)
		client.errs = []error{nil, nil, nil, nil, context.DeadlineExceeded}

		scraper.scrape(t.Context())

		require.NotZero(t, scraper.missing.len(), "the streams singled out must still be excluded")
		assert.Contains(t, buf.String(), "level=info "+excludedLogStream)
		assert.NotContains(t, buf.String(), "level=warn "+excludedLogStream,
			"a stream singled out while the group is gone would name an instance that is fine")
	})

	t.Run("warns once the group answers", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		streams := resourceIDs(10)
		scraper, client := fallingBack(t, &buf, streams...)
		delete(client.missing, streams[0])
		client.events = eventsFor(streams[0])

		scraper.scrape(t.Context())

		assert.Equal(t, len(streams)-1, scraper.missing.len())
		assert.Contains(t, buf.String(), "level=warn "+excludedLogStream,
			"a stream missing while its group answers is missing for a reason of its own")
	})
}

// TestScrapeRetriesTheStreamsExcludedWhileTheLogGroupWasInDoubt covers what a fallback bisect the
// deadline cut short leaves behind: streams singled out in a scrape nothing answered, whose
// rejections the group may have been all there was to.
func TestScrapeRetriesTheStreamsExcludedWhileTheLogGroupWasInDoubt(t *testing.T) { //nolint:funlen
	t.Parallel()

	// fallenBack returns a blamed session whose fallback bisect the deadline cut after four
	// rejections, with the streams the bisect singled out excluded.
	fallenBack := func(t *testing.T, streams ...string) (*scraper, *fakeLogsClient) {
		t.Helper()

		scraper, client := blamedGroupScraper(t, streams...)
		scraper.group.rejectedProbes = maxRejectedProbes
		scraper.group.probeAfter = time.Now().Add(-time.Minute)
		client.errs = []error{nil, nil, nil, nil, context.DeadlineExceeded}

		scraper.scrape(t.Context())

		require.NotZero(t, scraper.missing.len(), "the streams the bisect singled out must be excluded")

		return scraper, client
	}

	t.Run("as soon as the group answers", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(10)
		scraper, client := fallenBack(t, streams...)

		// The group is back with every stream in it. This scrape asks only for what was not excluded,
		// and that is what proves the group exists.
		client.missing = nil
		client.events = eventsFor(streams...)

		metrics, _ := scraper.scrape(t.Context())

		require.Less(t, len(metrics), len(streams), "the excluded streams were not asked for yet")
		assert.Zero(t, scraper.missing.len(),
			"an exclusion made while the group was in doubt must not outlive the group's answer")

		metrics, _ = scraper.scrape(t.Context())

		assert.Len(t, metrics, len(streams),
			"the instances behind them must report on the next scrape rather than wait a TTL for a probe slot")
	})

	t.Run("and excludes again the one that stays gone", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(10)
		scraper, client := fallenBack(t, streams...)

		gone := ""

		for _, stream := range streams {
			if scraper.missing.marked(stream) {
				gone = stream

				break
			}
		}

		require.NotEmpty(t, gone)

		client.missing = missingSet(gone)
		client.events = eventsFor(streams...)
		delete(client.events, gone)

		var buf bytes.Buffer

		scraper.logger = level.NewFilter(log.NewLogfmtLogger(&buf), level.AllowDebug())
		counted := scraper.errorCounts[errorKindNotFound]

		scraper.scrape(t.Context())

		require.False(t, scraper.missing.marked(gone), "the group answering releases every exclusion in doubt")

		metrics, _ := scraper.scrape(t.Context())

		assert.True(t, scraper.missing.marked(gone),
			"a stream still rejected while its group answers is missing for a reason of its own")
		assert.Equal(t, 1, scraper.missing.len())
		assert.Len(t, metrics, len(streams)-1)
		assert.Equal(t, counted+1, scraper.errorCounts[errorKindNotFound],
			"an exclusion released and made again is a second exclusion, and counts once more")
		assert.Equal(t, 1, strings.Count(buf.String(), "level=warn "+excludedLogStream),
			"the one stream missing for a reason of its own is warned about once")
	})
}

// TestScrapeKeepsAnExclusionTheLogGroupCannotHaveMade covers a bisect whose healthy half fails for
// a reason of its own once the missing streams are singled out. Nothing in the scrape was answered,
// but the group has answered before and is not blamed, so it is not what rejected them, and the
// group answering next cannot release them: the fleet would otherwise pay the bisect every other
// scrape for as long as the throttling lasted.
func TestScrapeKeepsAnExclusionTheLogGroupCannotHaveMade(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(10)
	gone, healthy := streams[:5], streams[5:]
	client := &fakeLogsClient{events: eventsFor(streams...), missing: nil, errs: nil, pageSize: 0, calls: nil}
	scraper := scraperWithStreams(client, streams...)

	scraper.scrape(t.Context())

	// The bisect singles the gone streams out in ten requests, then the request for the healthy half
	// is throttled.
	client.missing = missingSet(gone...)
	client.events = eventsFor(healthy...)

	client.errs = append(make([]error, 10), throttlingError())

	scraper.scrape(t.Context())

	require.Empty(t, client.errs, "the throttle must land on the request for the healthy half")
	require.Equal(t, len(gone), scraper.missing.len(), "the streams singled out are excluded")

	client.calls = nil
	metrics, _ := scraper.scrape(t.Context())

	assert.Len(t, client.calls, 1, "with the gone streams excluded the healthy half is one request")
	assert.Len(t, metrics, len(healthy))
	assert.Equal(t, len(gone), scraper.missing.len(),
		"the group answering says nothing about an exclusion the group cannot have made")

	client.calls = nil

	scraper.scrape(t.Context())

	assert.Len(t, client.calls, 1, "the bisect must not be paid again")
	assert.Equal(t, uint64(len(gone)), scraper.errorCounts[errorKindNotFound],
		"a stream excluded once is counted once")
}

// TestScrapeProbesTheLogGroupWithAStreamAlreadyExcluded covers a pause during which every stream
// is excluded and none is due: the probe must still ask one of them, since a stream that exists
// answers whatever the exclusion said, and asking nothing would hold the pause for good.
func TestScrapeProbesTheLogGroupWithAStreamAlreadyExcluded(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(10)
	scraper, client := blamedGroupScraper(t, streams...)

	for _, stream := range streams {
		scraper.missing.mark(stream, time.Now(), false)
	}

	scraper.group.probeAfter = time.Now().Add(-time.Minute)
	client.calls = nil

	scraper.scrape(t.Context())

	require.Len(t, client.calls, 1, "a due probe is requested whatever the streams are excluded on")
	assert.Equal(t, []string{streams[0]}, client.calls[0].streams,
		"with nothing better to ask the rotation starts over the whole fleet")
}

// TestScrapeProbesTheLogGroupPastTheStreamsAlreadyGone covers a pause during which some streams
// were excluded on their own evidence before the group went dark: those are gone whatever the group
// is doing, so a probe spent on one buys the whole session another TTL of silence and learns nothing.
// The rotation must skip them, or a fleet whose first few streams are gone would wait through a
// rejected probe per gone stream, and then a fallback bisect, to notice a group that is back.
func TestScrapeProbesTheLogGroupPastTheStreamsAlreadyGone(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(10)
	gone, alive := streams[:3], streams[3:]
	client := &fakeLogsClient{
		events:   eventsFor(alive...),
		missing:  missingSet(gone...),
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := scraperWithStreams(client, streams...)

	scraper.scrape(t.Context())

	for _, stream := range gone {
		require.True(t, scraper.missing.firm(stream), "a stream rejected while the group answered is excluded on its own evidence")
	}

	client.events = nil
	client.missing = missingSet(streams...)

	scraper.scrape(t.Context())

	require.True(t, scraper.group.paused(), "the group must be the one taking the blame")

	client.events = eventsFor(alive...)
	client.missing = missingSet(gone...)
	client.calls = nil
	scraper.group.probeAfter = time.Now().Add(-time.Minute)

	scraper.scrape(t.Context())

	require.NotEmpty(t, client.calls)
	assert.Equal(t, []string{alive[0]}, client.calls[0].streams, "the probe must name a stream the group could be blamed for")
	assert.False(t, scraper.group.paused(), "one answered probe ends the pause")

	client.calls = nil

	metrics, _ := scraper.scrape(t.Context())

	assert.Len(t, client.calls, 1, "the next scrape asks the fleet in one request, not a bisect")
	assert.Len(t, metrics, len(alive), "the instances whose streams exist report as soon as the pause ends")
	assert.Equal(t, len(gone), scraper.missing.len(), "the streams gone on their own evidence stay excluded")
}

// TestScrapeFallsBackOverTheWholeFleet covers a fallback while every stream is excluded in doubt:
// the bisect it hands the session to must ask the whole fleet, not only the streams a probe slot is
// due for, or the streams beyond the first maxProbesPerScrape in configuration order would never be
// asked while the first ones are genuinely gone.
func TestScrapeFallsBackOverTheWholeFleet(t *testing.T) {
	t.Parallel()

	streams := resourceIDs(10)
	gone, alive := streams[:maxProbesPerScrape], streams[maxProbesPerScrape:]
	scraper, client := blamedGroupScraper(t, streams...)

	for _, stream := range streams {
		scraper.missing.mark(stream, time.Now(), true)
	}

	client.missing = missingSet(gone...)
	client.events = eventsFor(alive...)

	var metrics map[instanceKey]instanceMetrics

	for range maxRejectedProbes + 1 {
		scraper.group.probeAfter = time.Now().Add(-time.Minute)

		metrics, _ = scraper.scrape(t.Context())
	}

	assert.Len(t, metrics, len(alive), "the instances whose streams exist must report once the fallback asks for them")
	assert.Equal(t, len(gone), scraper.missing.len(), "the streams that are gone are excluded on their own evidence")
	assert.True(t, scraper.group.probeAfter.IsZero(), "a group that answered is not blamed")
}

func TestScrapeDoesNotCountAThrottledLogGroupProbe(t *testing.T) {
	t.Parallel()

	scraper, client := blamedGroupScraper(t, resourceIDs(10)...)

	for range maxRejectedProbes + 1 {
		scraper.group.probeAfter = time.Now().Add(-time.Minute)
		client.errs = []error{throttlingError()}
		client.calls = nil

		scraper.scrape(t.Context())

		require.Len(t, client.calls, 1, "a due probe is one request, throttled or not")
		assert.Len(t, client.calls[0].streams, 1,
			"rate limiting alone must not talk the session into bisecting the whole fleet")
	}

	assert.Zero(t, scraper.group.rejectedProbes, "only a rejection counts against the group")
	assert.True(t, scraper.group.probeAfter.After(time.Now()),
		"a throttled probe has spent its turn, so the pause backs off rather than repeating every scrape")
}
