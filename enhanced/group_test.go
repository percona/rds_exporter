package enhanced

import (
	"bytes"
	"context"
	"math"
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

	blameGroup(t, scraper, client, streams...)

	return scraper, client
}

// blameGroup makes every one of the given log streams disappear at once and scrapes, which is the
// evidence that makes a session blame the log group rather than the streams. A session that held
// some of them excluded asks the whole fleet before it blames the group, so that takes it two.
func blameGroup(t *testing.T, scraper *scraper, client *fakeLogsClient, streams ...string) {
	t.Helper()

	client.events = nil
	client.missing = missingSet(streams...)

	scraper.scrape(t.Context())

	if scraper.sweep == sweepRequested {
		scraper.scrape(t.Context())
	}

	require.True(t, scraper.group.paused(), "the group must be the one taking the blame")
}

// makeProbeDue brings the next log group probe forward, so that a test need not wait out a TTL.
func makeProbeDue(scraper *scraper) {
	scraper.group.probeAfter = time.Now().Add(-time.Minute)
}

// probedStreams runs the given number of scrapes with the log group probe always due, and returns
// the stream each of them probed. A scrape that gave up on probing opens with the whole fleet
// instead of one stream, and contributes nothing.
func probedStreams(t *testing.T, scraper *scraper, client *fakeLogsClient, scrapes int) []string {
	t.Helper()

	probed := make([]string, 0, scrapes)

	for range scrapes {
		makeProbeDue(scraper)

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
		makeProbeDue(scraper)

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

// blamedGroupBehindADeadline returns a blamed session whose client can be made to cut every scrape
// at a given number of requests, the way the scrape deadline cuts a bisect that does not fit the
// interval. The cut is left off until the blame, which the scrapes leading up to it need answered.
func blamedGroupBehindADeadline(t *testing.T, streams ...string) (*scraper, *deadlineClient) {
	t.Helper()

	client := &deadlineClient{
		fakeLogsClient: &fakeLogsClient{
			events:   eventsFor(streams...),
			missing:  nil,
			errs:     nil,
			pageSize: 0,
			calls:    nil,
		},
		callsBeforeCut: math.MaxInt,
	}
	scraper := scraperWithStreams(client, streams...)

	scraper.scrape(t.Context())

	blameGroup(t, scraper, client.fakeLogsClient, streams...)

	return scraper, client
}

// fallBackAndRunOutOfTime gives the pause up for a fallback and cuts its bisect at the given request,
// which is what a fleet whose bisect does not fit the scrape interval does on every scrape.
func fallBackAndRunOutOfTime(t *testing.T, scraper *scraper, client *deadlineClient, cut int) {
	t.Helper()

	scraper.group.rejectedProbes = scraper.group.fallbackThreshold()
	makeProbeDue(scraper)

	client.calls = nil
	client.callsBeforeCut = cut

	scraper.scrape(t.Context())
}

func TestScrapeBlamesTheLogGroup(t *testing.T) {
	t.Parallel()

	t.Run("blames the log group when nothing answers", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(4)
		client := groupMissingClient(streams...)
		scraper := scraperWithStreams(client, streams...)

		scraper.scrape(t.Context())

		assert.Zero(t, scraper.missing.len(), "a stream that exists must not be excluded for the group's sake")
		assert.Equal(t, uint64(1), scraper.errorCounts[errorKindGroupNotFound],
			"the group is one problem, not one per instance")
		assert.Zero(t, scraper.errorCounts[errorKindNotFound])
		assert.True(t, scraper.group.paused(), "the group must be retried")
	})

	t.Run("stops requesting while the log group is missing", func(t *testing.T) {
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
	})

	t.Run("blames the log group across every batch", func(t *testing.T) {
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
	})

	t.Run("does not blame the log group without evidence", func(t *testing.T) {
		t.Parallel()

		t.Run("a batch of one stream", func(t *testing.T) {
			t.Parallel()

			client := groupMissingClient(missingResourceID)
			scraper := scraperWithStreams(client, missingResourceID)

			scraper.scrape(t.Context())

			assert.Equal(t, 1, scraper.missing.len(), "one stream cannot tell itself apart from its group")
			assert.Equal(t, uint64(1), scraper.errorCounts[errorKindNotFound])
			assert.Zero(t, scraper.errorCounts[errorKindGroupNotFound])

			for range 2 {
				scraper.scrape(t.Context())
			}

			assert.Zero(t, scraper.errorCounts[errorKindGroupNotFound], "a session this small never blames the group")
			assert.False(t, scraper.group.paused())
		})

		t.Run("one batch of several", func(t *testing.T) {
			t.Parallel()

			// A missing group would have rejected the other batch too, so a batch that is merely gone
			// says nothing about it.
			streams := resourceIDs(maxLogStreamsPerRequest + 1)
			client := groupMissingClient(streams[:maxLogStreamsPerRequest]...)
			client.events = eventsFor(streams[maxLogStreamsPerRequest])
			scraper := scraperWithStreams(client, streams...)

			metrics := scraper.scrape(t.Context())

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
			assert.False(t, scraper.group.paused(), "churn must not pause the whole session")

			for range 2 {
				scraper.scrape(t.Context())
			}

			assert.Zero(t, scraper.errorCounts[errorKindGroupNotFound], "a session this small never blames the group")
			assert.False(t, scraper.group.paused())
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
	})

	t.Run("does not blame the log group for a throttled scrape", func(t *testing.T) {
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
		assert.False(t, scraper.group.paused(), "a throttled scrape may not pause the whole session")
		assert.Equal(t, len(gone), scraper.missing.len(), "the streams singled out are missing either way")

		metrics := scraper.scrape(t.Context())

		assert.Len(t, metrics, len(healthy), "the batch that was throttled must report on the next scrape")
	})

	t.Run("reports a log group outage once", func(t *testing.T) {
		t.Parallel()

		scraper, client := blamedGroupScraper(t, resourceIDs(10)...)

		gaps := fallbackGaps(t, scraper, client, 8*scraper.group.fallbackThreshold())
		require.NotEmpty(t, gaps, "the fallbacks this is about must have happened")

		assert.Equal(t, uint64(1), scraper.errorCounts[errorKindGroupNotFound],
			"one group that went missing once is one error, however many fallbacks went looking for it")
		assert.Zero(t, scraper.missing.len(),
			"no instance may be named for a fault that belongs to the group")
		assert.True(t, scraper.group.paused(), "the group is still the one taking the blame")
	})

	// TestScrapeAsksTheStreamsHeldBackBeforeBlamingTheLogGroup covers a scrape that asked for the few
	// streams a probe slot was due for and was rejected everywhere. That is what a missing group looks
	// like, and also what maxProbesPerScrape streams that stayed gone look like while the exclusions
	// behind them, some of which may exist again, wait for a slot. The group may only take the blame
	// once the whole fleet has been asked.
	t.Run("blames the log group of a fleet excluded one stream at a time", func(t *testing.T) {
		t.Parallel()

		// A fleet that lost all but one of its streams while the group kept answering, so every loss
		// was excluded on its own evidence and the session asks for one stream, and then loses the
		// group too.
		streams := resourceIDs(maxLogStreamsPerRequest)
		last := streams[len(streams)-1]
		client := &fakeLogsClient{events: eventsFor(streams...), missing: nil, errs: nil, pageSize: 0, calls: nil}
		scraper := scraperWithStreams(client, streams...)

		scraper.scrape(t.Context())

		client.events = eventsFor(last)
		client.missing = missingSet(streams[:len(streams)-1]...)

		scraper.scrape(t.Context())

		require.Equal(t, len(streams)-1, scraper.missing.len())
		require.False(t, scraper.group.paused(), "a group that answered is not what rejected the streams")

		client.events = nil
		client.missing = missingSet(streams...)

		scraper.scrape(t.Context())

		require.False(t, scraper.group.paused(), "one stream cannot tell itself apart from its group")
		require.Equal(t, sweepNone, scraper.sweep)

		scraper.scrape(t.Context())

		require.Equal(t, sweepRequested, scraper.sweep, "a second rejection over the same stream asks for the fleet")
		require.False(t, scraper.group.paused())

		client.calls = nil

		scraper.scrape(t.Context())

		require.NotEmpty(t, client.calls)
		assert.Equal(t, streams, client.calls[0].streams, "the whole fleet is asked before the group is blamed")
		assert.True(t, scraper.group.paused(), "rejected over the fleet with nothing held back, the group takes the blame")
		assert.Equal(t, uint64(1), scraper.errorCounts[errorKindGroupNotFound])
	})

	t.Run("asks the streams held back before blaming the log group", func(t *testing.T) {
		t.Parallel()

		// excludedFleet returns a session that collected once and then had every one of its streams
		// excluded on its own evidence, all of them due for a probe slot at once.
		excludedFleet := func(t *testing.T, streams ...string) (*scraper, *fakeLogsClient) {
			t.Helper()

			client := &fakeLogsClient{events: eventsFor(streams...), missing: nil, errs: nil, pageSize: 0, calls: nil}
			scraper := scraperWithStreams(client, streams...)

			scraper.scrape(t.Context())

			for _, stream := range streams {
				scraper.missing.mark(stream, time.Now().Add(-2*missingStreamTTL), false)
			}

			client.calls = nil

			return scraper, client
		}

		t.Run("the streams behind the probe cap exist again", func(t *testing.T) {
			t.Parallel()

			streams := resourceIDs(maxProbesPerScrape + 4)
			gone, alive := streams[:maxProbesPerScrape], streams[maxProbesPerScrape:]
			scraper, client := excludedFleet(t, streams...)
			client.events = eventsFor(alive...)
			client.missing = missingSet(gone...)

			metrics := scraper.scrape(t.Context())

			require.Len(t, client.calls[0].streams, maxProbesPerScrape, "the first scrape asks the streams due a probe slot")
			require.Empty(t, metrics)
			assert.False(t, scraper.group.paused(), "streams held back have not been heard from, so the group is not blamed")
			assert.Zero(t, scraper.errorCounts[errorKindGroupNotFound])

			client.calls = nil

			metrics = scraper.scrape(t.Context())

			require.NotEmpty(t, client.calls)
			assert.Equal(t, streams, client.calls[0].streams, "the next scrape asks the whole fleet, excluded or not")
			assert.Len(t, metrics, len(alive), "the instances whose streams exist again report")
			assert.Equal(t, len(gone), scraper.missing.len(), "the streams still gone stay excluded")
			assert.False(t, scraper.group.paused())

			client.calls = nil

			scraper.scrape(t.Context())

			require.Len(t, client.calls, 1)
			assert.Equal(t, alive, client.calls[0].streams, "the sweep is one scrape; the exclusions renewed wait their TTL")
		})

		t.Run("the group is gone", func(t *testing.T) {
			t.Parallel()

			streams := resourceIDs(maxProbesPerScrape + 4)
			scraper, client := excludedFleet(t, streams...)
			client.events = nil
			client.missing = missingSet(streams...)

			scraper.scrape(t.Context())

			require.False(t, scraper.group.paused(), "the first scrape cannot speak for the streams it held back")

			client.calls = nil

			scraper.scrape(t.Context())

			assert.Equal(t, streams, client.calls[0].streams, "the whole fleet is asked")
			assert.True(t, scraper.group.paused(), "rejected everywhere with nothing held back, the group takes the blame")
			assert.Equal(t, uint64(1), scraper.errorCounts[errorKindGroupNotFound])
			assert.Zero(t, scraper.errorCounts[errorKindNotFound], "the exclusions renewed are not new exclusions")
		})
	})

	// TestScrapeBlamesTheLogGroupAcrossScrapesTheDeadlineCuts pins a fleet gone all at once whose bisect
	// does not fit the interval. Each scrape is cut at the same place, so no single scrape is ever
	// rejected over the whole fleet; the rejections have to add up across them, or the group is never
	// blamed and the bisect is paid every other scrape for good. They are only trusted to once the sweep
	// they first add up to has been cut as well: a sweep that fits settles the group's account on its own.
	t.Run("blames the log group across scrapes the deadline cuts", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(16)
		fake := &fakeLogsClient{events: eventsFor(streams...), missing: nil, errs: nil, pageSize: 0, calls: nil}
		// A full bisect of sixteen missing streams costs thirty requests; twelve single out five of them.
		client := &deadlineClient{fakeLogsClient: fake, callsBeforeCut: 12}
		scraper := scraperWithStreams(client, streams...)

		scraper.scrape(t.Context())

		fake.missing = missingSet(streams...)

		scrapes := 0
		for scrapes < 8 && !scraper.group.paused() {
			fake.calls = nil

			scraper.scrape(t.Context())

			scrapes++
		}

		require.True(t, scraper.group.paused(), "the rejections of the cut scrapes must add up to the fleet")
		assert.Equal(t, 5, scrapes, "two cut scrapes reach ten streams, the third is rejected over the rest and asks for "+
			"a sweep, the sweep is cut, and the fifth is rejected over the rest again")
		assert.Equal(t, uint64(1), scraper.errorCounts[errorKindGroupNotFound])

		fake.calls = nil

		scraper.scrape(t.Context())

		assert.Empty(t, fake.calls, "a blamed group is paused, not swept again")
	})

	// TestScrapeBlamesTheLogGroupOfAFleetTooLargeToBisectInOneScrape covers a fleet whose bisect costs
	// more requests than one scrape can spend. No scrape is ever rejected over the whole of it, so the
	// blame has to come from the rejections adding up, and that only happens if the set of streams still
	// to be rejected over shrinks: re-asking the streams already carried against the group would hand
	// back every scrape as much of the set as the probe slots give out, and a session whose budget is
	// smaller than that would never blame the group at all, paying a cut bisect every scrape for the
	// whole outage instead.
	t.Run("blames the log group of a fleet too large to bisect in one scrape", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(250)
		// Thirty requests is what a five second interval can spend; a full bisect of one batch of a
		// hundred costs a hundred and ninety-eight.
		scraper, client := fleetGoneBehindADeadline(t, 30, streams...)

		scrapes := scrapesToBlameTheGroup(t, scraper, client, 40)

		assert.Equal(t, 21, scrapes, "each cut scrape singles out what its budget reaches, so eighteen of them "+
			"leave a remainder small enough to be rejected over in one; the sweep that asks the excluded "+
			"streams too is cut as well, and the scrape after it is what blames the group")
		assert.Equal(t, uint64(1), scraper.errorCounts[errorKindGroupNotFound])

		client.calls = nil

		scraper.scrape(t.Context())

		require.Len(t, client.calls, 1, "a blamed group probes one stream rather than bisecting the fleet again")
		assert.Len(t, client.calls[0].streams, 1)
	})
}

func TestScrapeProbesTheBlamedLogGroup(t *testing.T) {
	t.Parallel()

	t.Run("probes the log group with one stream", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(4)
		client := groupMissingClient(streams...)
		scraper := scraperWithStreams(client, streams...)

		scraper.scrape(t.Context())

		makeProbeDue(scraper)

		client.calls = nil

		scraper.scrape(t.Context())

		require.Len(t, client.calls, 1, "one stream answers for the whole group")
		assert.Equal(t, streams[:1], client.calls[0].streams)
		assert.Zero(t, scraper.missing.len(), "a probe rejected for the group says nothing about its stream")
		assert.True(t, scraper.group.probeAfter.After(time.Now()), "a rejected probe must wait for the next turn")
	})

	t.Run("rotates the log group probe", func(t *testing.T) {
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
				if scraper.group.paused() {
					makeProbeDue(scraper)
				}

				metrics = scraper.scrape(t.Context())
			}

			assert.False(t, scraper.group.paused(), "a probe that was answered must end the pause")
			assert.True(t, scraper.missing.marked(streams[0]),
				"the stream that is really gone must be excluded once the group stops taking the blame")

			for _, stream := range streams[1:] {
				assert.NotEmpty(t, metrics[testKey(stream)], "an instance whose stream exists must report again")
			}
		})
	})

	// TestScrapeProbesTheLogGroupWithAStreamAlreadyExcluded covers a pause during which every stream
	// is excluded and none is due: the probe must still ask one of them, since a stream that exists
	// answers whatever the exclusion said, and asking nothing would hold the pause for good.
	t.Run("probes the log group with a stream already excluded", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(10)
		scraper, client := blamedGroupScraper(t, streams...)

		for _, stream := range streams {
			scraper.missing.mark(stream, time.Now(), false)
		}

		makeProbeDue(scraper)

		client.calls = nil

		scraper.scrape(t.Context())

		require.Len(t, client.calls, 1, "a due probe is requested whatever the streams are excluded on")
		assert.Equal(t, []string{streams[0]}, client.calls[0].streams,
			"with nothing better to ask the rotation starts over the whole fleet")
	})

	// TestScrapeProbesTheLogGroupPastTheStreamsAlreadyGone covers a pause during which some streams
	// were excluded on their own evidence before the group went dark: those are gone whatever the group
	// is doing, so a probe spent on one buys the whole session another TTL of silence and learns nothing.
	// The rotation must skip them, or a fleet whose first few streams are gone would wait through a
	// rejected probe per gone stream, and then a fallback bisect, to notice a group that is back. The
	// sweep that follows the group's answer asks them all the same, once, since an exclusion renewed
	// through an outage is not the evidence it was.
	t.Run("probes the log group past the streams already gone", func(t *testing.T) {
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

		blameGroup(t, scraper, client, streams...)

		client.events = eventsFor(alive...)
		client.missing = missingSet(gone...)
		client.calls = nil

		makeProbeDue(scraper)

		scraper.scrape(t.Context())

		require.NotEmpty(t, client.calls)
		assert.Equal(t, []string{alive[0]}, client.calls[0].streams, "the probe must name a stream the group could be blamed for")
		assert.False(t, scraper.group.paused(), "one answered probe ends the pause")

		client.calls = nil

		metrics := scraper.scrape(t.Context())

		require.NotEmpty(t, client.calls)
		assert.Equal(t, streams, client.calls[0].streams, "the next scrape asks the whole fleet, the streams excluded included")
		assert.Len(t, metrics, len(alive), "the instances whose streams exist report as soon as the pause ends")
		assert.Equal(t, len(gone), scraper.missing.len(), "the streams gone on their own evidence stay excluded")
		assert.Zero(t, scraper.errorCounts[errorKindNotFound]-uint64(len(gone)),
			"a stream excluded before the outage and still gone after it is counted once")
	})

	// TestScrapeProbesEveryStreamWithinATTL covers a fleet blamed together, as a blue/green switchover of
	// every instance in a session leaves it while CloudWatch has yet to create the new streams. The
	// streams then appear at their own pace, and one probe per TTL would hold the first of them back a
	// TTL per stream ahead of it in the rotation: the probes are spaced so that the rotation goes round
	// the fleet once per TTL instead.
	t.Run("probes every stream within a TTL", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(3)
		scraper, client := blamedGroupScraper(t, streams...)

		spacing := missingStreamTTL / time.Duration(len(streams))
		assert.WithinDuration(t, time.Now().Add(spacing), scraper.group.probeAfter, time.Second,
			"the first probe is due a third of a TTL after the blame")

		// The last stream of the rotation is the first to exist again.
		client.missing = missingSet(streams[:2]...)
		client.events = eventsFor(streams[2])

		for turn, stream := range streams {
			makeProbeDue(scraper)

			client.calls = nil

			metrics := scraper.scrape(t.Context())

			require.Len(t, client.calls, 1)
			assert.Equal(t, []string{stream}, client.calls[0].streams, "the rotation asks the streams in turn")

			if turn < len(streams)-1 {
				assert.WithinDuration(t, time.Now().Add(spacing), scraper.group.probeAfter, time.Second,
					"a rejected probe waits a third of a TTL, not a whole one")
				assert.Empty(t, metrics)

				continue
			}

			assert.False(t, scraper.group.paused(), "the stream that exists clears the group")
			assert.NotEmpty(t, metrics[testKey(stream)], "the instance behind it reports on the probe that found it")
		}
	})

	t.Run("does not count a throttled log group probe", func(t *testing.T) {
		t.Parallel()

		scraper, client := blamedGroupScraper(t, resourceIDs(10)...)

		for range scraper.group.fallbackThreshold() + 1 {
			makeProbeDue(scraper)

			client.errs = []error{throttlingError()}
			client.calls = nil

			scraper.scrape(t.Context())

			require.Len(t, client.calls, 1, "a due probe is one request, throttled or not")
			assert.Len(t, client.calls[0].streams, 1,
				"rate limiting alone must not talk the session into bisecting the whole fleet")
		}

		assert.Zero(t, scraper.group.rejectedProbes, "only a rejection counts against the group")
		assert.False(t, scraper.group.probeAfter.After(time.Now()),
			"a throttled probe was not heard from, so it must not cost every instance in the session another TTL")

		// The next scrape probes again without the test bringing the probe forward, and the rotation has
		// moved past the stream the throttled probe named.
		throttled := client.calls[0].streams
		client.calls = nil

		scraper.scrape(t.Context())

		require.Len(t, client.calls, 1, "a probe not heard from is asked again on the next scrape")
		assert.Len(t, client.calls[0].streams, 1)
		assert.NotEqual(t, throttled, client.calls[0].streams, "the rotation moves on from the stream the throttled probe named")
	})
}

func TestScrapeRecoversFromALogGroupOutage(t *testing.T) {
	t.Parallel()

	t.Run("recovers when the log group exists again", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(4)
		client := groupMissingClient(streams...)
		scraper := scraperWithStreams(client, streams...)

		scraper.scrape(t.Context())

		client.missing = map[string]struct{}{}
		client.events = eventsFor(streams...)

		makeProbeDue(scraper)

		metrics := scraper.scrape(t.Context())
		require.Empty(t, metrics[testKey(streams[1])], "the probe only asks for one stream")
		assert.False(t, scraper.group.paused(), "an answered request is all the evidence the group exists")

		client.calls = nil

		metrics = scraper.scrape(t.Context())

		require.Len(t, client.calls, 1)
		assert.Equal(t, streams, client.calls[0].streams, "the whole fleet is requested again")

		for _, stream := range streams {
			assert.NotEmpty(t, metrics[testKey(stream)])
		}
	})

	// TestScrapeRetriesEveryExclusionOnceTheLogGroupAnswers covers a fleet coming back from an outage
	// with every stream excluded on its own evidence: waiting for their probe slots would let them
	// return maxProbesPerScrape per scrape, over as many scrapes as it takes, for a fault the group's
	// answer has just explained away.
	t.Run("retries every exclusion once the log group answers", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(3 * maxProbesPerScrape)
		scraper, client := blamedGroupScraper(t, streams...)

		for _, stream := range streams {
			scraper.missing.mark(stream, time.Now(), false)
		}

		// The group is back with every stream in it.
		client.missing = nil
		client.events = eventsFor(streams...)

		makeProbeDue(scraper)

		scraper.scrape(t.Context())

		require.False(t, scraper.group.paused(), "an answered probe ends the pause")

		client.calls = nil

		metrics := scraper.scrape(t.Context())

		require.Len(t, client.calls, 1, "the whole fleet fits one request")
		assert.Equal(t, streams, client.calls[0].streams, "every exclusion is retried at once, due or not")
		assert.Len(t, metrics, len(streams), "every instance reports on the first scrape after the pause")
		assert.Zero(t, scraper.missing.len())
	})

	// TestScrapeReturnsAWholeFleetOnTheScrapeAfterTheLogGroupAnswers covers what the blame is worth to a
	// fleet that comes back: the answer sweeps every exclusion the outage made, so the instances behind
	// them report at once rather than a probe slot's worth per scrape.
	t.Run("returns a whole fleet on the scrape after the log group answers", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(250)
		scraper, client := fleetGoneBehindADeadline(t, 30, streams...)

		scrapesToBlameTheGroup(t, scraper, client, 40)

		client.missing = nil
		client.events = eventsFor(streams...)

		makeProbeDue(scraper)

		metrics := scraper.scrape(t.Context())

		require.Len(t, metrics, 1, "the probe asks one stream, and it answers")

		metrics = scraper.scrape(t.Context())

		assert.Len(t, metrics, len(streams), "every instance reports on the scrape after the group answered")
		assert.Zero(t, scraper.missing.len(), "the exclusions of the outage go with the group's answer")
	})

	// TestScrapeReleasesTheExclusionsOfAnOutageOnceTheLogGroupAnswers covers what holding them in doubt
	// is worth: the group's answer releases every exclusion the outage made at once, so the instances
	// behind them report on the next scrape rather than waiting out a probe slot each.
	t.Run("releases the exclusions of an outage once the log group answers", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(16)
		scraper, client := fleetGoneBehindADeadline(t, 12, streams...)

		scraper.scrape(t.Context())
		ageExclusions(scraper, scraper.interval())

		firm := scraper.missing.len()

		for range 3 {
			client.calls = nil

			scraper.scrape(t.Context())
			ageExclusions(scraper, scraper.interval())
		}

		require.Greater(t, scraper.missing.len(), firm, "the outage must have excluded more than its first scrape did")

		client.missing = nil
		client.events = eventsFor(streams...)
		client.callsBeforeCut = math.MaxInt

		makeProbeDue(scraper)

		scraper.scrape(t.Context())

		assert.Equal(t, firm, scraper.missing.len(),
			"the group answering leaves only the exclusions it cannot have been the cause of")

		metrics := scraper.scrape(t.Context())

		assert.Len(t, metrics, len(streams)-firm,
			"every instance whose exclusion waited on the group reports again without waiting a TTL out")
	})

	// TestScrapeRecoversAFirmExclusionTheDeadlineMadeByMistake covers the one way a firm exclusion is
	// wrong: a bisect the deadline cut on the first scrape of a group outage excludes the streams it
	// singled out on their own evidence, because the group had answered before and was not yet blamed,
	// though the group is what rejected them. Once the group is back and other streams are gone for real,
	// a scrape asking only for the unexcluded streams is rejected everywhere, and the sweep that follows
	// asks the wrongly excluded streams before the group can be blamed for what the gone ones did.
	t.Run("recovers a firm exclusion the deadline made by mistake", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(10)
		client := &fakeLogsClient{events: eventsFor(streams...), missing: nil, errs: nil, pageSize: 0, calls: nil}
		scraper := scraperWithStreams(client, streams...)

		scraper.scrape(t.Context())

		// The group goes dark and the deadline cuts the bisect after ten rejections, half way through.
		client.events = nil
		client.missing = missingSet(streams...)
		client.errs = []error{nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, context.DeadlineExceeded}

		scraper.scrape(t.Context())

		require.False(t, scraper.group.paused(), "a bisect the deadline cut is not the evidence that blames the group")

		var healthy, gone []string

		for _, stream := range streams {
			if scraper.missing.firm(stream) {
				healthy = append(healthy, stream)
			} else {
				gone = append(gone, stream)
			}
		}

		require.Len(t, healthy, len(streams)/2, "the streams singled out before the cut are excluded on their own evidence")

		// The group is back with the streams wrongly excluded, and the rest are gone for real.
		client.errs = nil
		client.missing = missingSet(gone...)
		client.events = eventsFor(healthy...)
		client.calls = nil

		metrics := scraper.scrape(t.Context())

		require.Equal(t, gone, client.calls[0].streams, "this scrape asks only the streams not excluded")
		require.Empty(t, metrics)
		assert.False(t, scraper.group.paused(), "the streams held back have not been heard from")

		client.calls = nil

		metrics = scraper.scrape(t.Context())

		assert.Equal(t, streams, client.calls[0].streams, "the sweep asks the whole fleet")
		assert.Len(t, metrics, len(healthy), "the instances wrongly excluded report again without waiting out a pause")
		assert.Equal(t, len(gone), scraper.missing.len(), "the streams gone for real are excluded in their place")
		assert.False(t, scraper.group.paused(), "a group that answered is not blamed")
	})
}

func TestScrapeFallsBackToTheLogStreams(t *testing.T) {
	t.Parallel()

	t.Run("isolates the streams its probes kept landing on", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(10)
		dead, alive := streams[:len(streams)-1], streams[len(streams)-1]
		scraper, client := blamedGroupScraper(t, streams...)

		// The group is back, and with it one stream that the rotation would not reach for another eight
		// probes -- each of which would buy the pause holding that instance back another TTL.
		client.missing = missingSet(dead...)

		client.events = eventsFor(alive)

		var metrics map[instanceKey]instanceMetrics

		for range scraper.group.fallbackThreshold() + 1 {
			makeProbeDue(scraper)

			metrics = scraper.scrape(t.Context())
		}

		assert.False(t, scraper.group.paused(), "a pause no probe answers must not outlive them")
		assert.Equal(t, len(dead), scraper.missing.len(), "the streams the probes landed on must be excluded")
		assert.NotEmpty(t, metrics[testKey(alive)], "the instance whose stream exists must report again")
	})

	// TestScrapeFallsBackForALogGroupThatNeverAnswered covers a session whose group was gone from its
	// first scrape. Its probes rotate one stream per scrape, so a fleet of which only the last stream
	// exists would wait a scrape per stream ahead of it to report; the fallback asks the whole fleet
	// after fallbackThreshold rejected probes, and backs off like a group that went dark, so that a
	// region that never enabled Enhanced Monitoring pays one bisect per few hours for it at most.
	t.Run("falls back for a log group that never answered", func(t *testing.T) {
		t.Parallel()

		t.Run("recovers the one stream that exists", func(t *testing.T) {
			t.Parallel()

			streams := resourceIDs(10)
			dead, alive := streams[:len(streams)-1], streams[len(streams)-1]
			client := groupMissingClient(streams...)
			scraper := scraperWithStreams(client, streams...)

			scraper.scrape(t.Context())

			require.True(t, scraper.group.paused())

			client.missing = missingSet(dead...)
			client.events = eventsFor(alive)

			var metrics map[instanceKey]instanceMetrics

			for range scraper.group.fallbackThreshold() + 1 {
				makeProbeDue(scraper)

				metrics = scraper.scrape(t.Context())
			}

			assert.False(t, scraper.group.paused(), "a pause no probe answers must not outlive them")
			assert.NotEmpty(t, metrics[testKey(alive)],
				"the instance whose stream exists must not wait a TTL for every stream ahead of it")
			assert.Equal(t, len(dead), scraper.missing.len(), "the streams the fallback found gone are excluded")
		})

		t.Run("backs off between fallbacks that find nothing", func(t *testing.T) {
			t.Parallel()

			streams := resourceIDs(10)
			client := groupMissingClient(streams...)
			scraper := scraperWithStreams(client, streams...)

			scraper.scrape(t.Context())

			threshold := scraper.group.fallbackThreshold()
			gaps := fallbackGaps(t, scraper, client, 8*threshold)

			require.GreaterOrEqual(t, len(gaps), 3, "three fallbacks must fit in these scrapes")
			assert.Equal(t, []int{threshold, 2 * threshold, 4 * threshold}, gaps[:3],
				"a region without Enhanced Monitoring is bisected less and less often")
			assert.Zero(t, scraper.missing.len(), "a fallback rejected everywhere names no stream")
			assert.Equal(t, uint64(1), scraper.errorCounts[errorKindGroupNotFound], "one group missing is one error")
		})
	})

	// TestScrapeFallsBackOverTheWholeFleet covers a fallback while every stream is excluded: the bisect
	// it hands the session to must ask the whole fleet, not only the streams a probe slot is due for, or
	// the streams beyond the first maxProbesPerScrape in configuration order would never be asked while
	// the first ones are genuinely gone. A firm exclusion is asked too, since it is not due at all, and
	// the pause it would otherwise wait out is one the group has just been cleared of. The answer that
	// clears it must not buy a second sweep: the fallback has asked everything there was to ask.
	t.Run("falls back over the whole fleet", func(t *testing.T) {
		t.Parallel()

		for _, testCase := range []struct {
			name      string
			tentative bool
		}{
			{name: "excluded in doubt", tentative: true},
			{name: "excluded on their own evidence", tentative: false},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				t.Parallel()

				streams := resourceIDs(10)
				gone, alive := streams[:maxProbesPerScrape], streams[maxProbesPerScrape:]
				scraper, client := blamedGroupScraper(t, streams...)

				for _, stream := range streams {
					scraper.missing.mark(stream, time.Now(), testCase.tentative)
				}

				client.missing = missingSet(gone...)
				client.events = eventsFor(alive...)

				var metrics map[instanceKey]instanceMetrics

				for range scraper.group.fallbackThreshold() + 1 {
					makeProbeDue(scraper)

					metrics = scraper.scrape(t.Context())
				}

				assert.Len(t, metrics, len(alive), "the instances whose streams exist must report once the fallback asks for them")
				assert.Equal(t, len(gone), scraper.missing.len(), "the streams that are gone are excluded on their own evidence")
				assert.False(t, scraper.group.paused(), "a group that answered is not blamed")

				client.calls = nil

				scraper.scrape(t.Context())

				require.Len(t, client.calls, 1, "the fallback was the sweep; the streams it found gone are not bisected again")
				assert.Equal(t, alive, client.calls[0].streams)
			})
		}
	})

	// TestScrapeFallsBackAfterATTLOfRejectedProbes covers a fleet too large for its scrape interval to
	// probe in full within a TTL. The probes come one per scrape, and the pause is given up after a TTL's
	// worth of them rather than after a fixed count, so that the fallback still asks the whole fleet
	// within a TTL of the blame.
	t.Run("falls back after a TTL of rejected probes", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(maxLogStreamsPerRequest)
		scraper, client := blamedGroupScraper(t, streams...)

		interval := scraper.interval()
		require.Greater(t, interval*time.Duration(len(streams)), missingStreamTTL,
			"the fleet must be too large to go round within a TTL for this test to mean anything")
		assert.WithinDuration(t, time.Now().Add(interval), scraper.group.probeAfter, time.Second,
			"probes come no more often than scrapes")

		probes := int(missingStreamTTL / interval)
		require.Equal(t, probes, scraper.group.fallbackThreshold())

		for range probes {
			makeProbeDue(scraper)

			client.calls = nil

			scraper.scrape(t.Context())

			require.Len(t, client.calls, 1)
			assert.Len(t, client.calls[0].streams, 1, "a TTL's worth of probes come first")
		}

		makeProbeDue(scraper)

		client.calls = nil

		scraper.scrape(t.Context())

		require.NotEmpty(t, client.calls)
		assert.Equal(t, streams, client.calls[0].streams, "a TTL of rejected probes hands the fleet to the bisect")
	})

	t.Run("stops paying for fallbacks that find nothing", func(t *testing.T) {
		t.Parallel()

		scraper, client := blamedGroupScraper(t, resourceIDs(10)...)

		threshold := scraper.group.fallbackThreshold()
		gaps := fallbackGaps(t, scraper, client, 8*threshold)

		require.GreaterOrEqual(t, len(gaps), 3, "three fallbacks must fit in these scrapes")
		assert.Equal(t, []int{threshold, 2 * threshold, 4 * threshold}, gaps[:3],
			"a fallback that found nothing makes the next one wait twice as long")
	})

	// TestScrapeKeepsTheLogGroupPausedWhenTheFallbackRunsOutOfTime covers the fallback of a fleet whose
	// bisect does not fit the scrape interval. The pause is given up so that the bisect can settle the
	// group's account, and one that runs out of time settles nothing: the group is still blamed, still
	// the likeliest explanation, and a pause left off would have every scrape after it pay the same
	// bisect for as long as the outage lasted, at the whole account's rate limit.
	t.Run("keeps the log group paused when the fallback runs out of time", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(16)
		scraper, client := blamedGroupBehindADeadline(t, streams...)

		// A full bisect of sixteen missing streams costs thirty requests, so twelve is a fallback cut
		// short of the streams it would take to blame the group again.
		fallBackAndRunOutOfTime(t, scraper, client, 12)

		require.True(t, scraper.group.blamed, "a fallback that heard nothing has not cleared the group")
		require.True(t, scraper.group.paused(), "a fallback that settled nothing leaves the pause its blame stands on")

		client.calls = nil

		scraper.scrape(t.Context())

		require.Len(t, client.calls, 1, "the scrape after the fallback probes rather than bisecting the fleet again")
		assert.Len(t, client.calls[0].streams, 1)
		assert.Equal(t, uint64(1), scraper.errorCounts[errorKindGroupNotFound],
			"the pause taken back is the same outage, and one outage is counted once")
	})

	// TestScrapeBacksOffTheFallbacksThatRanOutOfTime covers what the pause taken back is worth: a
	// fallback that ran out of time found nothing either, so the next one is worth less than the last,
	// and the wait for it doubles like any other fallback's. A pause resumed without that would buy a
	// bisect it cannot finish every threshold of probes, for the whole outage.
	t.Run("backs off the fallbacks that ran out of time", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(16)
		scraper, client := blamedGroupBehindADeadline(t, streams...)

		threshold := scraper.group.fallbackThreshold()

		for range 2 {
			fallBackAndRunOutOfTime(t, scraper, client, 12)

			require.True(t, scraper.group.paused(), "each fallback cut short must leave the pause behind it")
		}

		assert.Equal(t, 2, scraper.group.unproductiveFallbacks,
			"a fallback that was not answered anywhere found nothing, whether it finished or not")
		assert.Equal(t, threshold<<2, scraper.group.fallbackThreshold(),
			"the probes the next pause is given double for each fallback that found nothing")
	})
}

func TestScrapeExcludesStreamsWhileTheLogGroupIsInDoubt(t *testing.T) {
	t.Parallel()

	t.Run("warns about a missing log stream only once the log group answers", func(t *testing.T) {
		t.Parallel()

		// fallingBack returns a blamed session about to give its pause up for a bisect, logging into buf.
		fallingBack := func(t *testing.T, buf *bytes.Buffer, streams ...string) (*scraper, *fakeLogsClient) {
			t.Helper()

			scraper, client := blamedGroupScraper(t, streams...)
			scraper.logger = level.NewFilter(log.NewLogfmtLogger(buf), level.AllowDebug())
			scraper.group.rejectedProbes = scraper.group.fallbackThreshold()
			makeProbeDue(scraper)

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
	})

	// TestScrapeRetriesTheStreamsExcludedWhileTheLogGroupWasInDoubt covers what a fallback bisect the
	// deadline cut short leaves behind: streams singled out in a scrape nothing answered, whose
	// rejections the group may have been all there was to.
	t.Run("retries the streams excluded while the log group was in doubt", func(t *testing.T) {
		t.Parallel()

		// fallenBack returns a blamed session whose fallback bisect the deadline cut after four
		// rejections, with the streams the bisect singled out excluded.
		fallenBack := func(t *testing.T, streams ...string) (*scraper, *fakeLogsClient) {
			t.Helper()

			scraper, client := blamedGroupScraper(t, streams...)
			scraper.group.rejectedProbes = scraper.group.fallbackThreshold()
			makeProbeDue(scraper)

			client.errs = []error{nil, nil, nil, nil, context.DeadlineExceeded}

			scraper.scrape(t.Context())

			require.NotZero(t, scraper.missing.len(), "the streams the bisect singled out must be excluded")

			return scraper, client
		}

		t.Run("as soon as the group answers", func(t *testing.T) {
			t.Parallel()

			streams := resourceIDs(10)
			scraper, client := fallenBack(t, streams...)

			// The group is back with every stream in it. The pause the fallback could not settle is back
			// too, so the probe of this scrape is what proves the group exists.
			client.missing = nil
			client.events = eventsFor(streams...)

			metrics := scraper.scrape(t.Context())

			require.Less(t, len(metrics), len(streams), "the excluded streams were not asked for yet")
			assert.Zero(t, scraper.missing.len(),
				"an exclusion made while the group was in doubt must not outlive the group's answer")

			metrics = scraper.scrape(t.Context())

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

			// A fallback the deadline cut leaves the pause standing, so the group's answer arrives through
			// the probe rotation: rejected over the stream that stays gone, answered by the next one.
			for range streams {
				makeProbeDue(scraper)
				scraper.scrape(t.Context())

				if !scraper.group.blamed {
					break
				}
			}

			require.False(t, scraper.group.blamed, "a probe landing on a stream that exists clears the group")
			require.False(t, scraper.missing.marked(gone), "the group answering releases every exclusion in doubt")

			metrics := scraper.scrape(t.Context())

			assert.True(t, scraper.missing.marked(gone),
				"a stream still rejected while its group answers is missing for a reason of its own")
			assert.Equal(t, 1, scraper.missing.len())
			assert.Len(t, metrics, len(streams)-1)
			assert.Equal(t, counted+1, scraper.errorCounts[errorKindNotFound],
				"an exclusion released and made again is a second exclusion, and counts once more")
			assert.Equal(t, 1, strings.Count(buf.String(), "level=warn "+excludedLogStream),
				"the one stream missing for a reason of its own is warned about once")
		})
	})

	// TestScrapeKeepsAnExclusionTheLogGroupCannotHaveMade covers a bisect whose healthy half fails for
	// a reason of its own once the missing streams are singled out. Nothing in the scrape was answered,
	// but the group has answered before and is not blamed, so it is not what rejected them, and the
	// group answering next cannot release them: the fleet would otherwise pay the bisect every other
	// scrape for as long as the throttling lasted.
	t.Run("keeps an exclusion the log group cannot have made", func(t *testing.T) {
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
		metrics := scraper.scrape(t.Context())

		assert.Len(t, client.calls, 1, "with the gone streams excluded the healthy half is one request")
		assert.Len(t, metrics, len(healthy))
		assert.Equal(t, len(gone), scraper.missing.len(),
			"the group answering says nothing about an exclusion the group cannot have made")

		client.calls = nil

		scraper.scrape(t.Context())

		assert.Len(t, client.calls, 1, "the bisect must not be paid again")
		assert.Equal(t, uint64(len(gone)), scraper.errorCounts[errorKindNotFound],
			"a stream excluded once is counted once")
	})

	// TestScrapeHoldsInDoubtTheExclusionsOfAnOutageNoScrapeHeardTheEndOf covers a fleet gone all at once
	// whose bisect the deadline cuts on every scrape. Each cut scrape singles out more of it, and their
	// rejections are all being kept against the log group, because none of them could tell a fleet that
	// is gone from a group that is. Reading each of them as the streams' own evidence instead would warn
	// by name about every instance of a fleet the group is about to be blamed for.
	t.Run("holds in doubt the exclusions of an outage no scrape heard the end of", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		streams := resourceIDs(16)
		// A full bisect of sixteen missing streams costs thirty requests; twelve reach a third of them.
		scraper, client := fleetGoneBehindADeadline(t, 12, streams...)
		scraper.logger = level.NewFilter(log.NewLogfmtLogger(&buf), level.AllowDebug())

		scraper.scrape(t.Context())
		ageExclusions(scraper, scraper.interval())

		firm := scraper.missing.len()
		require.NotZero(t, firm, "the first scrape of an outage has nothing to say the group is what rejected it")

		for range 3 {
			client.calls = nil

			scraper.scrape(t.Context())
			ageExclusions(scraper, scraper.interval())
		}

		require.Greater(t, scraper.missing.len(), firm, "the scrapes after it single out more of the fleet")
		assert.Equal(t, firm, firmExclusions(scraper, streams),
			"an exclusion made while the outage's account was open waits on the group like the rejection it rests on")
		assert.Equal(t, firm, strings.Count(buf.String(), "level=warn "+excludedLogStream),
			"an instance is named only for an exclusion that rests on evidence about its own stream")
	})

	// TestScrapeAsksAProbeSlotsWorthOfAFleetCarriedInFull covers the one way leaving the carried streams
	// out of the request could go quiet: the rejections cover the fleet, so there is nothing left to ask,
	// but the scrape they covered it on was cut short and could not blame the group with them. Asking
	// nothing would leave the session with no way to close the account it is waiting on.
	t.Run("asks a probe slot's worth of a fleet carried in full", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(16)
		client := groupMissingClient(streams...)
		scraper := scraperWithStreams(client, streams...)

		for _, stream := range streams {
			scraper.missing.mark(stream, time.Now(), true)
			scraper.unansweredRejections[stream] = struct{}{}
		}

		asked := scraper.enhancedStreams(time.Now())

		assert.Len(t, asked, maxProbesPerScrape, "the fewest streams that can be rejected over in full are asked")
		assert.Subset(t, streams, asked)
	})
}

// fleetGoneBehindADeadline returns a session of the given size whose every stream disappeared after
// one answered scrape, and whose client cuts each scrape at the given number of requests: a fleet
// whose full bisect costs more requests than its scrape interval can spend, which is the ordinary
// case at the five requests per second FilterLogEvents allows an account.
func fleetGoneBehindADeadline(t *testing.T, cut int, streams ...string) (*scraper, *deadlineClient) {
	t.Helper()

	client := &deadlineClient{
		fakeLogsClient: &fakeLogsClient{
			events:   eventsFor(streams...),
			missing:  nil,
			errs:     nil,
			pageSize: 0,
			calls:    nil,
		},
		callsBeforeCut: cut,
	}
	scraper := scraperWithStreams(client, streams...)

	scraper.scrape(t.Context())

	client.events = nil
	client.missing = missingSet(streams...)

	return scraper, client
}

// ageExclusions spends one scrape interval of every wait a session is keeping, so that a test can
// run the scrapes of a long outage without waiting one out. What the passing time buys the session is
// probe slots: an exclusion whose TTL has run out is asked again, which is what a fleet still being
// bisected cannot afford.
func ageExclusions(scraper *scraper, interval time.Duration) {
	for stream, probeAfter := range scraper.missing.probeAfter {
		scraper.missing.probeAfter[stream] = probeAfter.Add(-interval)
	}

	if scraper.group.paused() {
		scraper.group.probeAfter = scraper.group.probeAfter.Add(-interval)
	}
}

// scrapesToBlameTheGroup scrapes until the log group is blamed, at most the given number of times,
// and returns how many it took. Each scrape spends an interval of the waits the last one left.
func scrapesToBlameTheGroup(t *testing.T, scraper *scraper, client *deadlineClient, limit int) int {
	t.Helper()

	scrapes := 0

	for scrapes < limit && !scraper.group.paused() {
		client.calls = nil

		scraper.scrape(t.Context())
		ageExclusions(scraper, scraper.interval())

		scrapes++
	}

	require.True(t, scraper.group.paused(), "the rejections of the cut scrapes must add up to the fleet")

	return scrapes
}

// firmExclusions counts the log streams excluded on evidence about themselves.
func firmExclusions(scraper *scraper, streams []string) int {
	firm := 0

	for _, stream := range streams {
		if scraper.missing.firm(stream) {
			firm++
		}
	}

	return firm
}

func TestLogGroupTakesNoPauseBackWithoutBlame(t *testing.T) {
	t.Parallel()

	now := time.Now()

	for _, testCase := range []struct {
		name  string
		group func() logGroup
		want  bool
	}{
		{
			name:  "a group nobody blamed has no pause to take back",
			group: newLogGroup,
			want:  false,
		},
		{
			name: "a group the fallback blamed again keeps the wait that blame set",
			group: func() logGroup {
				group := newLogGroup()
				group.blame(now, time.Minute)

				return group
			},
			want: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			group := testCase.group()
			probeAfter := group.probeAfter

			group.resumeAfterFallback(now, time.Minute)

			assert.Equal(t, testCase.want, group.paused())
			assert.Equal(t, probeAfter, group.probeAfter, "the pause a blame set must not be moved")
		})
	}
}
