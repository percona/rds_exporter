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

	t.Run("a bisect that ran out of budget", func(t *testing.T) {
		t.Parallel()

		streams := resourceIDs(maxLogStreamsPerRequest)
		client := groupMissingClient(streams...)
		scraper := scraperWithStreams(client, streams...)

		scraper.scrape(t.Context())

		assert.Zero(t, scraper.errorCounts[errorKindGroupNotFound],
			"a batch that was not attributed is no evidence about the group")
		assert.True(t, scraper.groupProbeAfter.IsZero())
	})
}
