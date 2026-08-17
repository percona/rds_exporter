package enhanced

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/percona/rds_exporter/sessions"
)

type instanceStateResolver interface {
	InstanceStates(ctx context.Context) (map[string]sessions.InstanceState, error)
}

const (
	resourceIDRefreshInterval = 5 * time.Minute
	logGroupName              = "RDSOSMetrics"

	// LogStreamNames accepts up to 100 items.
	// https://docs.aws.amazon.com/AmazonCloudWatchLogs/latest/APIReference/API_FilterLogEvents.html
	maxLogStreamsPerRequest = 100
)

// eventSink accumulates the metrics parsed out of log events, per instance and event timestamp.
type eventSink struct {
	metrics  map[string]map[time.Time][]prometheus.Metric
	messages map[string]map[time.Time]string
}

func newEventSink() *eventSink {
	return &eventSink{
		metrics:  make(map[string]map[time.Time][]prometheus.Metric),
		messages: make(map[string]map[time.Time]string),
	}
}

func (sink *eventSink) add(resourceID string, timestamp time.Time, metrics []prometheus.Metric, message string) {
	if sink.metrics[resourceID] == nil {
		sink.metrics[resourceID] = make(map[time.Time][]prometheus.Metric)
	}

	sink.metrics[resourceID][timestamp] = metrics

	if sink.messages[resourceID] == nil {
		sink.messages[resourceID] = make(map[time.Time]string)
	}

	sink.messages[resourceID][timestamp] = message
}

// times returns the event timestamps collected for each instance.
func (sink *eventSink) times() map[string][]time.Time {
	res := make(map[string][]time.Time, len(sink.metrics))
	for resourceID, events := range sink.metrics {
		res[resourceID] = make([]time.Time, 0, len(events))
		for timestamp := range events {
			res[resourceID] = append(res[resourceID], timestamp)
		}
	}

	return res
}

// latest returns the metrics and messages of the given timestamp for each instance.
func (sink *eventSink) latest(times map[string]time.Time) (map[string][]prometheus.Metric, map[string]string) {
	metrics := make(map[string][]prometheus.Metric, len(times))
	messages := make(map[string]string, len(times))

	for resourceID, timestamp := range times {
		metrics[resourceID] = sink.metrics[resourceID][timestamp]
		messages[resourceID] = sink.messages[resourceID][timestamp]
	}

	return metrics, messages
}

// scraper retrieves metrics from several RDS instances sharing a single session.
type scraper struct {
	instances             []sessions.Instance
	svc                   cloudwatchlogs.FilterLogEventsAPIClient
	stateResolver         instanceStateResolver
	nextResourceIDRefresh time.Time
	nextStartTime         time.Time
	logger                log.Logger

	testDisallowUnknownFields bool // for tests only
}

func newScraper(cfg aws.Config, instances []sessions.Instance, logger log.Logger) *scraper {
	return &scraper{
		instances:             instances,
		svc:                   cloudwatchlogs.NewFromConfig(cfg),
		stateResolver:         sessions.NewResourceIDResolver(cfg),
		nextResourceIDRefresh: time.Now().Add(resourceIDRefreshInterval).Round(0),
		nextStartTime:         time.Now().Add(-3 * time.Minute).Round(0), // strip monotonic clock reading
		logger:                log.With(logger, "component", "enhanced"),
	}
}

// enhancedStreams returns the log streams to request metrics from. Instances whose Enhanced
// Monitoring is disabled in AWS have no log stream at all, and CloudWatch rejects the whole
// request when any single requested stream does not exist.
func (s *scraper) enhancedStreams() []string {
	streams := make([]string, 0, len(s.instances))
	for _, instance := range s.instances {
		if instance.EnhancedMonitoringInterval <= 0 {
			continue
		}

		streams = append(streams, instance.ResourceID)
	}

	return streams
}

// start scrapes metrics in loop and sends them to the channel until context is canceled.
func (s *scraper) start(ctx context.Context, interval time.Duration, ch chan<- map[string][]prometheus.Metric) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// nothing
		case <-ctx.Done():
			return
		}

		scrapeCtx, cancel := context.WithTimeout(ctx, interval)
		m, _ := s.scrape(scrapeCtx)
		cancel()
		ch <- m
	}
}

// scrape performs a single scrape.
func (s *scraper) scrape(ctx context.Context) (map[string][]prometheus.Metric, map[string]string) {
	sink := newEventSink()

	err := s.refreshInstanceStates(ctx)
	if err != nil {
		level.Error(s.logger).Log("msg", "Failed to refresh RDS instance states.", "error", err)
	}

	for _, streams := range s.batches() {
		s.collectPages(ctx, streams, sink)
	}

	times, nextStartTime := betterTimes(sink.times())
	s.nextStartTime = nextStartTime

	return sink.latest(times)
}

// batches groups the log streams to request into requests CloudWatch accepts.
func (s *scraper) batches() [][]string {
	streams := s.enhancedStreams()

	batches := make([][]string, 0, len(streams)/maxLogStreamsPerRequest+1)
	for start := 0; start < len(streams); start += maxLogStreamsPerRequest {
		end := min(start+maxLogStreamsPerRequest, len(streams))
		batches = append(batches, streams[start:end])
	}

	return batches
}

// collectPages paginates a single FilterLogEvents request, keeping the events of every page
// fetched before an error.
func (s *scraper) collectPages(ctx context.Context, streams []string, sink *eventSink) {
	input := &cloudwatchlogs.FilterLogEventsInput{ //nolint:exhaustruct
		LogGroupName:   aws.String(logGroupName),
		LogStreamNames: streams,
		StartTime:      aws.Int64(s.nextStartTime.UnixMilli()),
	}

	level.Debug(log.With(s.logger,
		"next_start", s.nextStartTime.UTC(),
		"since_last", time.Since(s.nextStartTime),
		"batch_size", len(streams),
	)).Log("msg", "Requesting metrics")

	paginator := cloudwatchlogs.NewFilterLogEventsPaginator(s.svc, input)
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			level.Error(s.logger).Log("msg", "Failed to filter log events.", "error", err, "batch_size", len(streams))

			return
		}

		for _, event := range output.Events {
			s.handleEvent(event, sink)
		}
	}
}

// handleEvent parses a single log event and stores its metrics under the owning instance.
func (s *scraper) handleEvent(event types.FilteredLogEvent, sink *eventSink) {
	logger := log.With(s.logger,
		"EventId", aws.ToString(event.EventId),
		"LogStreamName", aws.ToString(event.LogStreamName),
		"Timestamp", time.UnixMilli(aws.ToInt64(event.Timestamp)).UTC(),
		"IngestionTime", time.UnixMilli(aws.ToInt64(event.IngestionTime)).UTC())

	instance := s.instanceFor(aws.ToString(event.LogStreamName))
	if instance == nil {
		level.Error(logger).Log("msg", "Failed to find instance.")

		return
	}

	if instance.DisableEnhancedMetrics {
		level.Debug(logger).Log("msg", fmt.Sprintf("Enhanced Metrics are disabled for instance %v.", instance))

		return
	}

	logger = log.With(logger, "region", instance.Region, "instance", instance.Instance)

	osMetrics, err := parseOSMetrics([]byte(aws.ToString(event.Message)), s.testDisallowUnknownFields)
	if err != nil {
		// only for tests
		if s.testDisallowUnknownFields {
			panic(fmt.Sprintf("New metrics should be added: %s", err))
		}

		level.Error(logger).Log("msg", "Failed to parse metrics.", "error", err)

		return
	}

	timestamp := time.UnixMilli(aws.ToInt64(event.Timestamp)).UTC()
	level.Debug(logger).Log("msg", fmt.Sprintf("Timestamp from message: %s; from event: %s.", osMetrics.Timestamp.UTC(), timestamp))

	sink.add(
		instance.ResourceID,
		timestamp,
		osMetrics.makePrometheusMetrics(instance.Region, instance.Labels),
		aws.ToString(event.Message),
	)
}

// instanceFor returns the instance owning the given log stream, or nil when no instance does.
func (s *scraper) instanceFor(logStreamName string) *sessions.Instance {
	for _, instance := range s.instances {
		if instance.ResourceID == logStreamName {
			return &instance
		}
	}

	return nil
}

func (s *scraper) refreshInstanceStates(ctx context.Context) error {
	if time.Now().Before(s.nextResourceIDRefresh) {
		return nil
	}

	s.nextResourceIDRefresh = time.Now().Add(resourceIDRefreshInterval).Round(0)

	return s.updateInstanceStates(ctx)
}

func (s *scraper) updateInstanceStates(ctx context.Context) error {
	states, err := s.stateResolver.InstanceStates(ctx)
	if err != nil {
		return fmt.Errorf("failed to refresh instance states: %w", err)
	}

	for instanceIndex, instance := range s.instances {
		state, ok := states[instance.Instance]
		if !ok || state.ResourceID == "" {
			level.Warn(s.logger).Log(
				"msg", "RDS resource ID not found.",
				"region", instance.Region,
				"instance", instance.Instance,
			)
			continue
		}

		s.updateMonitoringInterval(instanceIndex, state.MonitoringInterval)

		if state.ResourceID == instance.ResourceID {
			continue
		}

		level.Info(s.logger).Log(
			"msg", "RDS resource ID changed.",
			"region", instance.Region,
			"instance", instance.Instance,
			"resource_id", state.ResourceID,
		)
		s.instances[instanceIndex].ResourceID = state.ResourceID
	}

	return nil
}

// updateMonitoringInterval records a change of the instance's Enhanced Monitoring state, which
// decides whether the instance has a log stream to request at all.
func (s *scraper) updateMonitoringInterval(instanceIndex int, interval time.Duration) {
	instance := s.instances[instanceIndex]
	if instance.EnhancedMonitoringInterval == interval {
		return
	}

	level.Info(s.logger).Log(
		"msg", "RDS Enhanced Monitoring interval changed.",
		"region", instance.Region,
		"instance", instance.Instance,
		"interval", interval,
	)
	s.instances[instanceIndex].EnhancedMonitoringInterval = interval
}

// betterTimes returns timestamps of the latest metrics, and also StarTime that should be used in the next request
func betterTimes(allTimes map[string][]time.Time) (times map[string]time.Time, nextStartTime time.Time) {
	// keep only the most recent metrics for each instance
	nextStartTime = time.Now()
	times = make(map[string]time.Time) // ResourceID -> timestamp
	for resourceID, events := range allTimes {
		var newest time.Time
		for _, timestamp := range events {
			if newest.Before(timestamp) {
				newest = timestamp
				times[resourceID] = timestamp
			}
		}

		if nextStartTime.After(newest) {
			nextStartTime = newest
		}
	}

	return
}
