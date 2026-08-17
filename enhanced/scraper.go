package enhanced

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

	// maxLookback bounds how far back a request may reach after a failed scrape or an outage.
	maxLookback = 3 * time.Minute

	// maxFutureSkew is how far ahead of the exporter's own clock an event may be timestamped. It
	// tolerates clock drift between the exporter host and AWS, and nothing more.
	maxFutureSkew = time.Minute
)

// instanceKey identifies an instance independently of its RDS resource ID, which changes on a
// blue/green switchover.
type instanceKey struct {
	region   string
	instance string
}

func keyOf(instance sessions.Instance) instanceKey {
	return instanceKey{region: instance.Region, instance: instance.Instance}
}

// instanceMetrics is one instance's most recent Enhanced Monitoring sample.
type instanceMetrics struct {
	metrics   []prometheus.Metric
	eventTime time.Time
}

// eventSink accumulates the metrics parsed out of log events, per instance and event timestamp.
type eventSink struct {
	metrics  map[instanceKey]map[time.Time][]prometheus.Metric
	messages map[instanceKey]map[time.Time]string
}

func newEventSink() *eventSink {
	return &eventSink{
		metrics:  make(map[instanceKey]map[time.Time][]prometheus.Metric),
		messages: make(map[instanceKey]map[time.Time]string),
	}
}

func (sink *eventSink) add(key instanceKey, timestamp time.Time, metrics []prometheus.Metric, message string) {
	if sink.metrics[key] == nil {
		sink.metrics[key] = make(map[time.Time][]prometheus.Metric)
	}

	sink.metrics[key][timestamp] = metrics

	if sink.messages[key] == nil {
		sink.messages[key] = make(map[time.Time]string)
	}

	sink.messages[key][timestamp] = message
}

func (sink *eventSink) times() map[instanceKey][]time.Time {
	res := make(map[instanceKey][]time.Time, len(sink.metrics))
	for key, events := range sink.metrics {
		res[key] = make([]time.Time, 0, len(events))
		for timestamp := range events {
			res[key] = append(res[key], timestamp)
		}
	}

	return res
}

func (sink *eventSink) latest(times map[instanceKey]time.Time) (map[instanceKey]instanceMetrics, map[instanceKey]string) {
	metrics := make(map[instanceKey]instanceMetrics, len(times))
	messages := make(map[instanceKey]string, len(times))

	for key, timestamp := range times {
		metrics[key] = instanceMetrics{metrics: sink.metrics[key][timestamp], eventTime: timestamp}
		messages[key] = sink.messages[key][timestamp]
	}

	return metrics, messages
}

// scraper retrieves metrics from several RDS instances sharing a single session.
type scraper struct {
	instances             []sessions.Instance
	svc                   cloudwatchlogs.FilterLogEventsAPIClient
	stateResolver         instanceStateResolver
	missing               *missingStreams
	isolationCalls        int
	errorCounts           map[string]uint64
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
		missing:               newMissingStreams(),
		isolationCalls:        0,
		errorCounts:           make(map[string]uint64),
		nextResourceIDRefresh: time.Now().Add(resourceIDRefreshInterval).Round(0),
		nextStartTime:         time.Now().Add(-maxLookback).Round(0), // strip monotonic clock reading
		logger:                log.With(logger, "component", "enhanced"),
	}
}

// enhancedStreams returns the log streams to request metrics from. Instances whose Enhanced
// Monitoring is disabled in AWS have no log stream at all, and streams CloudWatch already reported
// as missing are left out until their probe is due, because CloudWatch rejects the whole request
// when any single requested stream does not exist.
func (s *scraper) enhancedStreams(now time.Time) []string {
	streams := make([]string, 0, len(s.instances))
	probes := 0

	for _, instance := range s.instances {
		if instance.EnhancedMonitoringInterval <= 0 {
			continue
		}

		if s.missing.marked(instance.ResourceID) {
			// Re-probes are staggered so that a fleet of missing streams cannot fill a whole batch.
			if probes >= maxProbesPerScrape || !s.missing.due(instance.ResourceID, now) {
				continue
			}

			probes++
		}

		streams = append(streams, instance.ResourceID)
	}

	return streams
}

type scrapeResult struct {
	metrics     map[instanceKey]instanceMetrics
	errorCounts map[string]uint64 // error kind -> occurrences during the scrape
	region      string
	interval    time.Duration
}

// interval returns how often to scrape, following the shortest Enhanced Monitoring interval AWS
// reports for the session.
func (s *scraper) interval() time.Duration {
	interval := maxInterval
	for _, instance := range s.instances {
		if instance.EnhancedMonitoringInterval > 0 && instance.EnhancedMonitoringInterval < interval {
			interval = instance.EnhancedMonitoringInterval
		}
	}

	return max(interval, minInterval)
}

// start scrapes metrics in loop and sends them to the channel until context is canceled. It owns
// the channel, so the receiver's range loop ends when the scraper stops.
func (s *scraper) start(ctx context.Context, results chan<- scrapeResult) {
	interval := s.interval()
	ticker := time.NewTicker(interval)

	defer ticker.Stop()
	defer close(results)

	for {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}

		metrics := s.scrapeOnce(ctx, interval)

		if !s.send(ctx, results, s.result(metrics)) {
			return
		}

		interval = s.retune(interval, ticker)
	}
}

// scrapeOnce bounds a scrape by the interval it belongs to. Isolating missing log streams costs
// extra requests per batch, so without a deadline one scrape of a region where nothing exists could
// keep running while the collector waits for it.
func (s *scraper) scrapeOnce(ctx context.Context, interval time.Duration) map[instanceKey]instanceMetrics {
	scrapeCtx, cancel := context.WithTimeout(ctx, interval)
	defer cancel()

	metrics, _ := s.scrape(scrapeCtx)

	return metrics
}

// send delivers a result unless the scraper is stopping, so shutting down cannot block on a channel
// nobody drains any more.
func (s *scraper) send(ctx context.Context, results chan<- scrapeResult, result scrapeResult) bool {
	select {
	case results <- result:
		return true
	case <-ctx.Done():
		return false
	}
}

// retune follows a change of the Enhanced Monitoring interval AWS reports. Turning Enhanced
// Monitoring on, or lowering its interval, would otherwise be ignored until the exporter restarts.
func (s *scraper) retune(interval time.Duration, ticker *time.Ticker) time.Duration {
	current := s.interval()
	if current == interval {
		return interval
	}

	level.Info(s.logger).Log("msg", "Enhanced metrics update interval changed.", "interval", current)
	ticker.Reset(current)

	return current
}

// result packages a scrape for the collector and resets the error counters.
func (s *scraper) result(metrics map[instanceKey]instanceMetrics) scrapeResult {
	counts := s.errorCounts
	s.errorCounts = make(map[string]uint64)

	return scrapeResult{metrics: metrics, errorCounts: counts, region: s.region(), interval: s.interval()}
}

func (s *scraper) region() string {
	if len(s.instances) == 0 {
		return ""
	}

	return s.instances[0].Region
}

// scrape performs a single scrape.
func (s *scraper) scrape(ctx context.Context) (map[instanceKey]instanceMetrics, map[instanceKey]string) {
	sink := newEventSink()

	err := s.refreshInstanceStates(ctx)
	if err != nil {
		level.Error(s.logger).Log("msg", "Failed to refresh RDS instance states.", "error", err)
	}

	var scrapeErr error

	for _, streams := range s.batches(time.Now()) {
		batchErr := s.collectBatch(ctx, streams, sink)
		if batchErr == nil {
			continue
		}

		scrapeErr = errors.Join(scrapeErr, batchErr)
		kind := errorKind(batchErr)
		s.errorCounts[kind]++

		level.Error(s.logger).Log("msg", "Failed to collect enhanced metrics.",
			"error", batchErr, "kind", kind, "batch_size", len(streams))

		if isContextError(batchErr) {
			break
		}
	}

	times, oldestNewest, collected := newestEventTimes(sink.times())
	s.advanceStartTime(oldestNewest, collected && scrapeErr == nil)

	return sink.latest(times)
}

// advanceStartTime moves the request window forward only when every batch reported. A failed scrape
// that moved it would permanently skip the events it did not read, and the window is clamped so that
// recovering from a long outage cannot make the next request paginate through hours of events.
func (s *scraper) advanceStartTime(oldestNewest time.Time, complete bool) {
	if complete && oldestNewest.After(s.nextStartTime) {
		s.nextStartTime = oldestNewest
	}

	if earliest := time.Now().Add(-maxLookback); s.nextStartTime.Before(earliest) {
		s.nextStartTime = earliest.Round(0) // strip monotonic clock reading
	}
}

func (s *scraper) batches(now time.Time) [][]string {
	streams := s.enhancedStreams(now)

	batches := make([][]string, 0, len(streams)/maxLogStreamsPerRequest+1)
	for start := 0; start < len(streams); start += maxLogStreamsPerRequest {
		end := min(start+maxLogStreamsPerRequest, len(streams))
		batches = append(batches, streams[start:end])
	}

	return batches
}

// collectBatch collects the events of the given log streams. CloudWatch fails the whole request
// when any single stream does not exist, so the batch is halved until the missing streams are
// identified and excluded, which keeps the remaining instances reporting.
func (s *scraper) collectBatch(ctx context.Context, streams []string, sink *eventSink) error {
	err := s.collectPages(ctx, streams, sink)
	if err == nil || !isResourceNotFound(err) {
		return err
	}

	// Each batch gets its own budget, so a batch where every stream is missing cannot stop the
	// batches after it from finding and excluding theirs.
	s.isolationCalls = 0

	return s.isolateMissing(ctx, streams, sink)
}

// isolateMissing halves a rejected batch until it can attribute the rejection to single log
// streams, spending at most maxIsolationCalls requests per scrape. What it cannot attribute this
// time is retried on the next scrape with a fresh budget.
func (s *scraper) isolateMissing(ctx context.Context, streams []string, sink *eventSink) error {
	if len(streams) == 1 {
		s.markMissing(streams[0])

		return nil
	}

	mid := len(streams) / halves

	return errors.Join(
		s.isolateHalf(ctx, streams[:mid], sink),
		s.isolateHalf(ctx, streams[mid:], sink),
	)
}

func (s *scraper) isolateHalf(ctx context.Context, streams []string, sink *eventSink) error {
	if s.isolationCalls >= maxIsolationCalls {
		return errIsolationBudget
	}

	s.isolationCalls++

	err := s.collectPages(ctx, streams, sink)
	if err == nil || !isResourceNotFound(err) {
		return err
	}

	return s.isolateMissing(ctx, streams, sink)
}

// markMissing excludes a log stream from later requests. It reports and logs only the transition, so
// that a permanently missing stream neither inflates the counter nor floods the log every scrape.
func (s *scraper) markMissing(logStreamName string) {
	if !s.missing.mark(logStreamName, time.Now()) {
		return
	}

	s.errorCounts[errorKindNotFound]++

	level.Warn(s.logger).Log(
		"msg", "CloudWatch log stream does not exist; excluding it from Enhanced Monitoring requests.",
		"log_stream", logStreamName,
		"instance", s.instanceNameFor(logStreamName),
	)
}

// collectPages paginates a single FilterLogEvents request, keeping the events of every page
// fetched before an error.
func (s *scraper) collectPages(ctx context.Context, streams []string, sink *eventSink) error {
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
			return fmt.Errorf("failed to filter log events: %w", err)
		}

		for _, event := range output.Events {
			s.handleEvent(event, sink)
		}
	}

	s.rearmProbes(streams)

	return nil
}

// rearmProbes gives another TTL to the excluded streams CloudWatch accepted but that delivered no
// event. handleEvent clears the ones that reported, so what is left here exists without publishing,
// and would otherwise stay due and spend one of the scrape's probe slots forever.
func (s *scraper) rearmProbes(streams []string) {
	now := time.Now()

	for _, stream := range streams {
		if s.missing.marked(stream) {
			s.missing.mark(stream, now)
		}
	}
}

// instanceNameFor returns the DB instance identifiers using the given log stream, for logging.
func (s *scraper) instanceNameFor(logStreamName string) string {
	names := make([]string, 0, 1)
	for _, instance := range s.instancesFor(logStreamName) {
		names = append(names, instance.Instance)
	}

	return strings.Join(names, ",")
}

func (s *scraper) handleEvent(event types.FilteredLogEvent, sink *eventSink) {
	logger := log.With(s.logger,
		"EventId", aws.ToString(event.EventId),
		"LogStreamName", aws.ToString(event.LogStreamName),
		"Timestamp", time.UnixMilli(aws.ToInt64(event.Timestamp)).UTC(),
		"IngestionTime", time.UnixMilli(aws.ToInt64(event.IngestionTime)).UTC())

	logStreamName := aws.ToString(event.LogStreamName)

	instances := s.instancesFor(logStreamName)
	if len(instances) == 0 {
		level.Error(logger).Log("msg", "Failed to find instance.")

		return
	}

	if s.missing.clear(logStreamName) {
		level.Info(logger).Log("msg", "CloudWatch log stream is back; resuming Enhanced Monitoring requests.",
			"log_stream", logStreamName)
	}

	timestamp := time.UnixMilli(aws.ToInt64(event.Timestamp)).UTC()
	if !s.usableTimestamp(timestamp, logger) {
		return
	}

	osMetrics, err := parseOSMetrics([]byte(aws.ToString(event.Message)), s.testDisallowUnknownFields)
	if err != nil {
		// only for tests
		if s.testDisallowUnknownFields {
			panic(fmt.Sprintf("New metrics should be added: %s", err))
		}

		level.Error(logger).Log("msg", "Failed to parse metrics.", "error", err)

		return
	}

	// Several configured instances can share a resource ID, and each of them needs its own sample.
	for _, instance := range instances {
		if instance.DisableEnhancedMetrics {
			level.Debug(logger).Log("msg", fmt.Sprintf("Enhanced Metrics are disabled for instance %v.", instance))

			continue
		}

		instanceLogger := log.With(logger, "region", instance.Region, "instance", instance.Instance)
		level.Debug(instanceLogger).Log("msg", fmt.Sprintf("Timestamp from message: %s; from event: %s.",
			osMetrics.Timestamp.UTC(), timestamp))

		sink.add(
			keyOf(instance),
			timestamp,
			osMetrics.makePrometheusMetrics(instance.Region, instance.Labels),
			aws.ToString(event.Message),
		)
	}
}

// usableTimestamp reports whether an event timestamp can be trusted. CloudWatch accepts log events
// dated up to two hours ahead, and the timestamp drives both the next request window and the cache
// entry's expiry, so one future-dated event would otherwise stall the whole session's requests and
// suppress every later sample of the instance it belongs to.
func (s *scraper) usableTimestamp(timestamp time.Time, logger log.Logger) bool {
	if !timestamp.After(time.Now().Add(maxFutureSkew)) {
		return true
	}

	s.errorCounts[errorKindFutureEvent]++

	level.Warn(logger).Log("msg", "Ignoring Enhanced Monitoring event timestamped in the future.")

	return false
}

func (s *scraper) instancesFor(logStreamName string) []sessions.Instance {
	res := make([]sessions.Instance, 0, 1)

	for _, instance := range s.instances {
		if instance.ResourceID == logStreamName {
			res = append(res, instance)
		}
	}

	return res
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

		// The retired resource ID will never come back, and the new one deserves a fresh attempt.
		s.missing.clear(instance.ResourceID)
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

	if interval <= 0 {
		// The stream is not requested at all any more, so its exclusion must not outlive it:
		// re-enabling Enhanced Monitoring would otherwise wait for a probe to come due.
		s.missing.clear(instance.ResourceID)
	}

	s.instances[instanceIndex].EnhancedMonitoringInterval = interval
}

// newestEventTimes returns the newest event timestamp per instance, the oldest of those timestamps,
// and whether any events were collected at all. The oldest is where the next request has to start,
// and when nothing was collected the caller keeps its current start time.
func newestEventTimes(allTimes map[instanceKey][]time.Time) (map[instanceKey]time.Time, time.Time, bool) {
	times := make(map[instanceKey]time.Time, len(allTimes))

	var oldestNewest time.Time

	for key, events := range allTimes {
		var newest time.Time
		for _, timestamp := range events {
			if newest.Before(timestamp) {
				newest = timestamp
				times[key] = timestamp
			}
		}

		if oldestNewest.IsZero() || oldestNewest.After(newest) {
			oldestNewest = newest
		}
	}

	return times, oldestNewest, len(times) > 0
}
