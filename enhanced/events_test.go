package enhanced

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/percona/rds_exporter/sessions"
)

// scraperLoggingTo returns a scraper writing debug-level logs into buf.
func scraperLoggingTo(buf *bytes.Buffer, client *fakeLogsClient, instances []sessions.Instance) *scraper {
	scraper := newTestScraperWithClient(client, instances)
	scraper.logger = level.NewFilter(log.NewLogfmtLogger(buf), level.AllowDebug())

	return scraper
}

func TestScrapeIgnoresEventForUnknownStream(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{
		events:   eventsFor(oldResourceID, "orphan-resource-id"),
		missing:  nil,
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	// The orphan stream is returned by AWS but owned by no configured instance.
	client.events[oldResourceID] = append(client.events[oldResourceID], client.events["orphan-resource-id"]...)
	scraper := scraperWithStreams(client, oldResourceID)

	metrics, _ := scraper.scrape(t.Context())

	assert.Len(t, metrics, 1)
	assert.NotEmpty(t, metrics[testKey(oldResourceID)])
}

func TestScrapeAttributesEventToEveryInstanceSharingResourceID(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{
		events:   eventsFor(sameResourceID),
		missing:  nil,
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := newTestScraperWithClient(client, []sessions.Instance{
		testInstance(blueGreenPrimaryInstance, sameResourceID),
		testInstance(unchangedPrimaryInstance, sameResourceID),
	})

	metrics, _ := scraper.scrape(t.Context())

	assert.NotEmpty(t, metrics[testKey(blueGreenPrimaryInstance)])
	assert.NotEmpty(t, metrics[testKey(unchangedPrimaryInstance)],
		"two instances sharing a resource ID must both get the sample")
}

func TestScrapeSkipsDisabledInstance(t *testing.T) {
	t.Parallel()

	disabled := testInstance(unchangedPrimaryInstance, sameResourceID)
	disabled.DisableEnhancedMetrics = true

	client := &fakeLogsClient{
		events:   eventsFor(oldResourceID, sameResourceID),
		missing:  nil,
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}
	scraper := newTestScraperWithClient(client, []sessions.Instance{
		testInstance(blueGreenPrimaryInstance, oldResourceID),
		disabled,
	})

	metrics, _ := scraper.scrape(t.Context())

	assert.NotEmpty(t, metrics[testKey(blueGreenPrimaryInstance)])
	assert.Empty(t, metrics[testKey(unchangedPrimaryInstance)])
}

func TestScrapeDoesNotLetLogStreamNamesForgeLogLines(t *testing.T) {
	t.Parallel()

	forged := "db-x\nlevel=error msg=\"forged\" caller=\"nowhere\""
	client := &fakeLogsClient{
		events:   eventsFor(forged),
		missing:  nil,
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}

	var buf bytes.Buffer

	scraper := scraperLoggingTo(&buf, client, []sessions.Instance{testInstance(blueGreenPrimaryInstance, forged)})

	scraper.scrape(t.Context())

	require.NotEmpty(t, buf.String())

	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		assert.NotEqual(t, `level=error msg="forged" caller="nowhere"`, line,
			"an AWS-controlled stream name must not be able to inject a log line")
	}
}

func TestScrapeDoesNotLogEventMessage(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{
		events:   eventsFor(oldResourceID),
		missing:  nil,
		errs:     nil,
		pageSize: 0,
		calls:    nil,
	}

	var buf bytes.Buffer

	scraper := scraperLoggingTo(&buf, client, []sessions.Instance{
		testInstance(blueGreenPrimaryInstance, oldResourceID),
	})

	scraper.scrape(t.Context())

	// Enhanced Monitoring documents carry process lists and command lines.
	assert.NotContains(t, buf.String(), "cpuUtilization")
	assert.NotContains(t, buf.String(), "instanceResourceID")
}

func TestStartClosesChannelOnContextCancel(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{events: nil, missing: nil, errs: nil, pageSize: 0, calls: nil}
	scraper := scraperWithStreams(client, oldResourceID)

	ctx, cancel := context.WithCancel(t.Context())
	results := make(chan scrapeResult)

	go scraper.start(ctx, time.Millisecond, results)

	cancel()

	for range results { //nolint:revive
		// drain until start closes the channel
	}
}

func TestUnknownStreamDoesNotPanic(t *testing.T) {
	t.Parallel()

	scraper := scraperWithStreams(&fakeLogsClient{events: nil, missing: nil, errs: nil, pageSize: 0, calls: nil})
	sink := newEventSink()

	scraper.handleEvent(types.FilteredLogEvent{}, sink) //nolint:exhaustruct

	assert.Empty(t, sink.metrics)
}
