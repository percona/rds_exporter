package enhanced

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"

	"github.com/percona/rds_exporter/sessions"
)

// logCall records a single FilterLogEvents request made by the scraper.
type logCall struct {
	streams   []string
	startTime int64
}

// fakeLogsClient is a scriptable cloudwatchlogs.FilterLogEventsAPIClient.
type fakeLogsClient struct {
	events   map[string][]types.FilteredLogEvent // log stream name -> events it holds
	missing  map[string]struct{}                 // streams CloudWatch does not know about
	errs     []error                             // popped one per call; a nil entry responds normally
	pageSize int                                 // 0 returns every matching event in a single page

	calls []logCall
}

// FilterLogEvents implements cloudwatchlogs.FilterLogEventsAPIClient.
func (c *fakeLogsClient) FilterLogEvents(
	ctx context.Context,
	input *cloudwatchlogs.FilterLogEventsInput,
	_ ...func(*cloudwatchlogs.Options),
) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	c.record(input)

	err := ctx.Err()
	if err != nil {
		return nil, fmt.Errorf("fake CloudWatch Logs client: %w", err)
	}

	if len(c.errs) > 0 {
		err, c.errs = c.errs[0], c.errs[1:]
		if err != nil {
			return nil, err
		}
	}

	// CloudWatch rejects the whole request when any single requested stream does not exist,
	// which is what lets one missing stream starve every other instance.
	for _, stream := range input.LogStreamNames {
		if _, ok := c.missing[stream]; ok {
			return nil, &types.ResourceNotFoundException{ //nolint:exhaustruct
				Message: aws.String("The specified log stream does not exist."),
			}
		}
	}

	return c.page(c.matchingEvents(input), input.NextToken)
}

// record adds a request to the call log. A wrapper that fails the call instead of answering it
// records it here too, so that what the tests count is every request the scraper made, answered or
// not, and the count a wrapper decides on cannot disagree with the log a test reads.
func (c *fakeLogsClient) record(input *cloudwatchlogs.FilterLogEventsInput) {
	c.calls = append(c.calls, logCall{
		streams:   slices.Clone(input.LogStreamNames),
		startTime: aws.ToInt64(input.StartTime),
	})
}

// deadlineClient answers like the fake it wraps until a scrape has made callsBeforeCut requests, and
// then fails every request the way the scrape deadline does, so that a bisect is cut at the same place
// on every scrape without a clock in the test. The fake is embedded rather than held in a field
// because the count it cuts on is the fake's own call log, which a test resets between scrapes and
// reads to see what the cut scrape asked for.
type deadlineClient struct {
	*fakeLogsClient

	callsBeforeCut int
}

// FilterLogEvents implements cloudwatchlogs.FilterLogEventsAPIClient.
func (c *deadlineClient) FilterLogEvents(
	ctx context.Context,
	input *cloudwatchlogs.FilterLogEventsInput,
	opts ...func(*cloudwatchlogs.Options),
) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	if len(c.calls) < c.callsBeforeCut {
		return c.fakeLogsClient.FilterLogEvents(ctx, input, opts...)
	}

	c.record(input)

	return nil, fmt.Errorf("fake CloudWatch Logs client: %w", context.DeadlineExceeded)
}

// recordingClient keeps the newest raw message of every log stream the client it wraps answers with,
// for the live test that regenerates the JSON fixtures from what CloudWatch actually returned.
type recordingClient struct {
	inner    cloudwatchlogs.FilterLogEventsAPIClient
	messages map[string]string
	newest   map[string]int64
}

// FilterLogEvents implements cloudwatchlogs.FilterLogEventsAPIClient.
func (c *recordingClient) FilterLogEvents(
	ctx context.Context,
	input *cloudwatchlogs.FilterLogEventsInput,
	opts ...func(*cloudwatchlogs.Options),
) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	out, err := c.inner.FilterLogEvents(ctx, input, opts...)
	if err != nil {
		return nil, err //nolint:wrapcheck // a recorder must hand the error on untouched
	}

	for _, event := range out.Events {
		stream := aws.ToString(event.LogStreamName)
		if timestamp := aws.ToInt64(event.Timestamp); timestamp >= c.newest[stream] {
			c.newest[stream] = timestamp
			c.messages[stream] = aws.ToString(event.Message)
		}
	}

	return out, nil
}

// matchingEvents returns the events of the requested streams that are not older than StartTime.
func (c *fakeLogsClient) matchingEvents(input *cloudwatchlogs.FilterLogEventsInput) []types.FilteredLogEvent {
	startTime := aws.ToInt64(input.StartTime)

	res := make([]types.FilteredLogEvent, 0, len(input.LogStreamNames))
	for _, stream := range input.LogStreamNames {
		for _, event := range c.events[stream] {
			if aws.ToInt64(event.Timestamp) >= startTime {
				res = append(res, event)
			}
		}
	}

	sort.SliceStable(res, func(i, j int) bool {
		if name := aws.ToString(res[i].LogStreamName); name != aws.ToString(res[j].LogStreamName) {
			return name < aws.ToString(res[j].LogStreamName)
		}

		return aws.ToInt64(res[i].Timestamp) < aws.ToInt64(res[j].Timestamp)
	})

	return res
}

// page returns the events addressed by token, plus a token for the next page if any remain.
func (c *fakeLogsClient) page(events []types.FilteredLogEvent, token *string) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	offset := 0
	if token != nil {
		_, err := fmt.Sscanf(aws.ToString(token), "page-%d", &offset)
		if err != nil {
			return nil, fmt.Errorf("malformed page token %q: %w", aws.ToString(token), err)
		}
	}

	end := len(events)
	if c.pageSize > 0 && offset+c.pageSize < end {
		end = offset + c.pageSize
	}

	out := &cloudwatchlogs.FilterLogEventsOutput{ //nolint:exhaustruct
		Events: events[min(offset, len(events)):end],
	}
	if end < len(events) {
		out.NextToken = aws.String(fmt.Sprintf("page-%d", end))
	}

	return out, nil
}

// testEventTime returns the event timestamp the hermetic tests are built around. It is relative to
// now because the scraper clamps its request window to maxLookback.
func testEventTime() time.Time {
	return time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
}

// testInstance returns an instance with Enhanced Monitoring enabled in AWS.
func testInstance(name, resourceID string) sessions.Instance {
	return sessions.Instance{
		Region:                     testRegion,
		Instance:                   name,
		DisableBasicMetrics:        false,
		DisableEnhancedMetrics:     false,
		ResourceID:                 resourceID,
		Labels:                     nil,
		EnhancedMonitoringInterval: time.Minute,
	}
}

// osMetricsEvent returns a log event carrying the smallest OS metrics document that parses.
func osMetricsEvent(resourceID string, timestamp time.Time) types.FilteredLogEvent {
	message := fmt.Sprintf(
		`{"engine":"MySQL","instanceID":"%s","instanceResourceID":"%s","numVCPUs":2,`+
			`"timestamp":"%s","uptime":"1:00:00","version":1,"cpuUtilization":{"total":10}}`,
		resourceID, resourceID, timestamp.UTC().Format(time.RFC3339),
	)

	return types.FilteredLogEvent{
		EventId:       aws.String(resourceID + "-" + timestamp.UTC().Format(time.RFC3339)),
		LogStreamName: aws.String(resourceID),
		Timestamp:     aws.Int64(timestamp.UnixMilli()),
		IngestionTime: aws.Int64(timestamp.Add(10 * time.Second).UnixMilli()),
		Message:       aws.String(message),
	}
}
