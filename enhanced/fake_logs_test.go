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
	c.calls = append(c.calls, logCall{
		streams:   slices.Clone(input.LogStreamNames),
		startTime: aws.ToInt64(input.StartTime),
	})

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
