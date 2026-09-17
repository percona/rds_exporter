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

	// maxLookback bounds how far back a request may reach after a failed scrape or an outage. Events
	// timestamped further behind the exporter's clock than this are never requested at all, so an
	// instance whose clock lags that much reports a gap rather than samples.
	maxLookback = 3 * time.Minute

	// refreshBackoffFactor is how much longer each consecutive failed state refresh waits, growing
	// from one scrape interval up to resourceIDRefreshInterval and back to zero on the first success.
	refreshBackoffFactor = 2

	// clockSkewReportThreshold is how far ahead of the exporter's own clock an event may be
	// timestamped and still be taken at face value: within it the timestamp is the ordinary drift of
	// two clocks and the event is as current as it claims; beyond it the skew is reported, and the
	// event is passed over for the newest one the clock does explain, or stored on its own when there
	// is none. It decides nothing about whether an instance keeps its sample: a future timestamp is
	// kept out of the request window and out of the sample's expiry by clamping both to now, so no
	// value of this constant can cost one. Anything short enough to catch ordinary drift would
	// otherwise have blanked every instance at once.
	clockSkewReportThreshold = time.Minute
)

// instanceKey identifies an instance independently of its RDS resource ID, which changes on a
// blue/green switchover. The session is part of it because a DB identifier is only unique within an
// AWS account: two accounts monitored in one region can each have an instance of the same name, and
// they are different instances with different samples.
type instanceKey struct {
	session  string
	region   string
	instance string
}

func keyOf(session string, instance sessions.Instance) instanceKey {
	return instanceKey{session: session, region: instance.Region, instance: instance.Instance}
}

// instanceMetrics is one instance's most recent Enhanced Monitoring sample, as a scrape found it.
// The collector stores it as a storedSample, which adds the expiry a scrape does not decide.
type instanceMetrics struct {
	metrics   []prometheus.Metric
	eventTime time.Time
}

// eventSink accumulates the metrics parsed out of log events, per instance and event timestamp.
type eventSink struct {
	metrics map[instanceKey]map[time.Time][]prometheus.Metric
}

func newEventSink() *eventSink {
	return &eventSink{
		metrics: make(map[instanceKey]map[time.Time][]prometheus.Metric),
	}
}

func (sink *eventSink) add(key instanceKey, timestamp time.Time, metrics []prometheus.Metric) {
	if sink.metrics[key] == nil {
		sink.metrics[key] = make(map[time.Time][]prometheus.Metric)
	}

	sink.metrics[key][timestamp] = metrics
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

func (sink *eventSink) latest(times map[instanceKey]time.Time) map[instanceKey]instanceMetrics {
	metrics := make(map[instanceKey]instanceMetrics, len(times))

	for key, timestamp := range times {
		metrics[key] = instanceMetrics{metrics: sink.metrics[key][timestamp], eventTime: timestamp}
	}

	return metrics
}

// scrapeEvidence is what one scrape gathers towards attributing its rejections: the streams it
// singled out, how many streams the rejected batches held, whether anything at all was answered, and
// the isolation budget of the batch under way. Every scrape begins it afresh.
type scrapeEvidence struct {
	isolationCalls  int
	isolated        []string
	rejectedStreams int
	answered        bool
}

func (e *scrapeEvidence) reset() {
	e.isolationCalls = 0
	e.isolated = e.isolated[:0]
	e.rejectedStreams = 0
	e.answered = false
}

// sweepState is where a session stands with the sweep, the one scrape that asks for every monitored
// stream, exclusions and probe cap notwithstanding. It is requested by the one kind of news that
// undermines the exclusions all at once -- the group's account has changed, so the evidence they rest
// on has to be gathered again -- and taken up by the scrape that follows. A fallback is under way as
// one from the start, and the news it brings must not buy the fleet a second.
type sweepState int

const (
	sweepNone sweepState = iota
	sweepRequested
	sweepUnderWay
)

// scraper retrieves metrics from several RDS instances sharing a single session.
type scraper struct {
	session       string
	instances     []sessions.Instance
	svc           cloudwatchlogs.FilterLogEventsAPIClient
	stateResolver instanceStateResolver
	missing       *missingStreams
	evidence      scrapeEvidence
	// unansweredRejections holds the streams singled out by the scrapes nothing has answered since,
	// so that what one scrape could not finish saying about the group is not lost to the next. A
	// bisect the deadline cuts singles out the streams it reached and never gets to say whether the
	// group rejected them; the next scrape asks the rest, and only with both does the rejection cover
	// the fleet. Anything answered empties it: from then on a stream rejected is rejected on its own.
	unansweredRejections map[string]struct{}
	// sweepCutShort is whether a sweep since anything last answered was cut by the deadline. It is
	// what lets the rejections carried above stand in for a sweep: a fleet that is all gone and does
	// not fit the interval can be rejected over in full only across scrapes, but the first time the
	// pieces add up the group is still owed the one request that could clear it.
	sweepCutShort         bool
	sweep                 sweepState
	group                 logGroup
	errorCounts           map[string]uint64
	skewedEvents          uint64
	nextResourceIDRefresh time.Time
	refreshBackoff        time.Duration
	nextStartTime         time.Time
	logger                log.Logger

	testDisallowUnknownFields bool // for tests only
}

func newScraper(session string, cfg aws.Config, instances []sessions.Instance, logger log.Logger) *scraper {
	return &scraper{
		session:               session,
		instances:             instances,
		svc:                   cloudwatchlogs.NewFromConfig(cfg),
		stateResolver:         sessions.NewInstanceStateResolver(cfg),
		missing:               newMissingStreams(),
		evidence:              scrapeEvidence{isolationCalls: 0, isolated: nil, rejectedStreams: 0, answered: false},
		unansweredRejections:  make(map[string]struct{}),
		sweepCutShort:         false,
		sweep:                 sweepNone,
		group:                 newLogGroup(),
		errorCounts:           make(map[string]uint64),
		skewedEvents:          0,
		nextResourceIDRefresh: time.Now().Add(resourceIDRefreshInterval).Round(0),
		refreshBackoff:        0,
		nextStartTime:         time.Now().Add(-maxLookback).Round(0),
		logger:                log.With(logger, "component", "enhanced"),
	}
}

// monitoredStreams returns the log stream of every instance whose Enhanced Monitoring is on, once
// each: instances configured more than once share one log stream. An instance whose Enhanced
// Monitoring is disabled in AWS has no log stream at all.
func (s *scraper) monitoredStreams() []string {
	streams := make([]string, 0, len(s.instances))
	listed := make(map[string]struct{}, len(s.instances))

	for _, instance := range s.instances {
		if instance.EnhancedMonitoringInterval <= 0 {
			continue
		}

		if _, seen := listed[instance.ResourceID]; seen {
			continue
		}

		listed[instance.ResourceID] = struct{}{}
		streams = append(streams, instance.ResourceID)
	}

	return streams
}

// enhancedStreams returns the log streams to request metrics from: the monitored streams less those
// CloudWatch already reported as missing, which are left out until their probe is due, because
// CloudWatch rejects the whole request when any single requested stream does not exist. A sweep asks
// for all of them once: the exclusions it overrides were made under a verdict on the group that has
// since changed, and a stream that is still gone costs the bisect once rather than the TTL and the
// probe slot it would otherwise wait for.
//
// A stream carried against the log group's open account is left out whether its probe is due or not.
// It is excluded and still counted towards blaming the group, so asking it again settles nothing and
// costs the rest of the fleet the budget its rejection spends: a fleet too large to bisect within one
// scrape would otherwise get back, every scrape, as many streams as the probe slots hand out, and the
// set of streams still to be rejected over would stop shrinking before the group could be blamed for
// any of them. Left out, the set shrinks by what the scrape's bisect reaches, until one scrape is
// rejected over the whole of what is left and the carried rejections can be cashed in. The account is
// emptied by anything answering, which is what makes a stream its own suspect again.
func (s *scraper) enhancedStreams(now time.Time) []string {
	monitored := s.monitoredStreams()
	if s.sweep == sweepUnderWay {
		return monitored
	}

	streams := make([]string, 0, len(monitored))
	open := make([]string, 0, len(monitored))
	probes := 0

	for _, stream := range monitored {
		if s.missing.marked(stream) {
			if _, carried := s.unansweredRejections[stream]; carried {
				open = append(open, stream)

				continue
			}

			// Re-probes are staggered so that a fleet of missing streams cannot fill a whole batch. A
			// stream shared by several instances spends one probe slot, not one per instance.
			if probes >= maxProbesPerScrape || !s.missing.due(stream, now) {
				continue
			}

			probes++
		}

		streams = append(streams, stream)
	}

	// A fleet carried in full leaves nothing to ask, and that is the one way leaving the carried
	// streams out could go quiet: the scrape whose rejections completed the group's account was cut
	// short, so it could not close the account itself, and a session with no request to make has
	// nothing that could close it either. A probe slot's worth is asked instead of none -- few enough
	// to be rejected over in full within one scrape, which is what blaming the group takes, and few
	// enough that the fleet is not bisected again to learn it.
	if len(streams) == 0 {
		return open[:min(len(open), maxProbesPerScrape)]
	}

	return streams
}

type scrapeResult struct {
	metrics      map[instanceKey]instanceMetrics
	errorCounts  map[string]uint64
	skewedEvents uint64
	monitored    map[instanceKey]bool
	region       string
	interval     time.Duration
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

	return s.scrape(scrapeCtx)
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
	skewed := s.skewedEvents
	s.skewedEvents = 0

	return scrapeResult{
		metrics:      metrics,
		errorCounts:  counts,
		skewedEvents: skewed,
		monitored:    s.monitoredInstances(),
		region:       s.region(),
		interval:     s.interval(),
	}
}

// monitoredInstances reports which instances AWS currently has Enhanced Monitoring on for. The
// collector cannot assert an instance is down when enhancedStreams has no log stream to request for
// it, and only the scrape goroutine may read the instances, so the state travels with the result.
func (s *scraper) monitoredInstances() map[instanceKey]bool {
	monitored := make(map[instanceKey]bool, len(s.instances))

	for _, instance := range s.instances {
		key := keyOf(s.session, instance)
		// Duplicate configurations share a key, and one of them having a stream is enough.
		monitored[key] = monitored[key] || instance.EnhancedMonitoringInterval > 0
	}

	return monitored
}

func (s *scraper) region() string {
	if len(s.instances) == 0 {
		return ""
	}

	return s.instances[0].Region
}

// scrape performs a single scrape.
func (s *scraper) scrape(ctx context.Context) map[instanceKey]instanceMetrics {
	sink := newEventSink()

	err := s.refreshInstanceStates(ctx)
	if err != nil {
		level.Error(s.logger).Log("msg", "Failed to refresh RDS instance states.", "error", err)
	}

	var scrapeErr error

	s.beginAttribution()

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

	// Blaming the group needs every request this scrape made to have been rejected over existence.
	// A batch that was throttled, refused or cut short by the deadline was not heard from at all, and
	// what it would have said is exactly what tells a missing group from a few missing streams. That
	// only limits what can be said about the group: a stream singled out by a rejection is missing
	// either way.
	s.attributeRejections(scrapeErr == nil)

	// result resets the count as it hands the scrape over, so this is what this scrape saw.
	if s.skewedEvents > 0 {
		level.Warn(s.logger).Log("msg", "Enhanced Monitoring events are timestamped in the future; "+
			"check the clock of this host against AWS.", "events", s.skewedEvents)
	}

	times, oldestNewest, collected := newestEventTimes(sink.times(), time.Now())
	s.advanceStartTime(oldestNewest, collected && windowMayAdvance(scrapeErr))

	return sink.latest(times)
}

// windowMayAdvance reports whether what a scrape collected may move the request window. A batch
// rejected for a reason of its own is asked again on the next scrape, so keeping the window still
// costs one scrape and loses nothing. A scrape that ran out of time is the opposite case: it would
// be handed back the window it could not drain, run out of time on it again, and never report at
// all, so the events it did read have to move the window even though the rest of the fleet did not
// report. Only what is behind the new start and was never read is lost, bounded by maxLookback.
//
// Both can happen in one scrape, and then the first rule wins: a batch that was throttled before a
// later one hit the deadline still has its events to deliver, and moving the window past them would
// drop them for good rather than for one scrape. What that costs is bounded the same way, since the
// window is clamped to maxLookback however long the two keep coinciding.
func windowMayAdvance(scrapeErr error) bool {
	return scrapeErr == nil || onlyContextErrors(scrapeErr)
}

// advanceStartTime moves the request window forward when the scrape is entitled to move it, and the
// window is clamped at both ends: recovering from a long outage cannot make the next request
// paginate through hours of events, and an event timestamped in the future cannot push the window
// past events that have yet to arrive. Clamping down only ever widens the window, because
// FilterLogEvents StartTime is inclusive.
func (s *scraper) advanceStartTime(oldestNewest time.Time, mayAdvance bool) {
	if mayAdvance && oldestNewest.After(s.nextStartTime) {
		s.nextStartTime = oldestNewest
	}

	now := time.Now()

	// Round(0) strips the monotonic clock reading whichever bound the window lands on.
	s.nextStartTime = notBefore(notAfter(s.nextStartTime, now), now.Add(-maxLookback)).Round(0)
}

func (s *scraper) batches(now time.Time) [][]string {
	if probe, presumedMissing := s.groupProbe(now); presumedMissing {
		return probe
	}

	streams := s.enhancedStreams(now)

	batches := make([][]string, 0, len(streams)/maxLogStreamsPerRequest+1)
	for start := 0; start < len(streams); start += maxLogStreamsPerRequest {
		end := min(start+maxLogStreamsPerRequest, len(streams))
		batches = append(batches, streams[start:end])
	}

	return batches
}

// groupProbe answers what to request while the log group itself is presumed missing: nothing until
// the probe is due, and then a single stream. A missing group rejects every request it is asked for,
// so requesting the whole fleet would pay a full bisect to learn what one stream already says.
//
// The rotation runs over every monitored stream the group could be blamed for, whether a probe slot
// is due for it or not. An exclusion made while the group was blamed rests on the group, and the
// stream answers as soon as the group is back; rotating over the streams due for a probe slot instead
// would stop at the first maxProbesPerScrape of them in configuration order, and never reach the rest
// while those happened to be gone. A firm exclusion is the one thing left out: it was made while the
// group answered, so the stream is gone whatever the group is doing, and a probe spent on it costs
// every instance in the session another TTL of silence to learn nothing.
func (s *scraper) groupProbe(now time.Time) ([][]string, bool) {
	candidates := s.probeCandidates()
	stream, decision := s.group.probe(candidates, now, s.probeSpacing(len(candidates)))

	switch decision {
	case probeWaiting:
		return nil, true
	case probeDue:
		return [][]string{{stream}}, true
	case probeGivenUp:
		s.resumeUnprobed()

		return nil, false
	case probeNotPaused:
		return nil, false
	}

	return nil, false
}

// probeSpacing is the wait between the probes of a paused session: no shorter than the scrape
// interval, since a probe is a request and nothing is requested more often than that, and no longer
// than lets the rotation go round every candidate within one TTL, so that a stream the group is
// wrongly blamed for waits at most the TTL a stream excluded on its own evidence would.
func (s *scraper) probeSpacing(candidates int) time.Duration {
	if candidates == 0 {
		return missingStreamTTL
	}

	return max(s.interval(), missingStreamTTL/time.Duration(candidates))
}

// currentProbeSpacing is the spacing a pause beginning now would probe at, over the candidates the
// fleet has at this moment. The probe path has its candidate list in hand already; the paths that
// blame the group or take its pause back ask for it here so that the two cannot drift apart.
func (s *scraper) currentProbeSpacing() time.Duration {
	return s.probeSpacing(len(s.probeCandidates()))
}

// probeCandidates returns the monitored streams a log group probe may name, in configuration order so
// that the rotation cursor keeps advancing over the same list from one scrape to the next. A firm
// exclusion is left out on trust: one made by mistake, on a stream a bisect the deadline cut singled
// out before the group was blamed, is indistinguishable from a right one, and the stream it names
// waits for the sweep that ends the pause instead of a probe. That is one pause of delay for a
// mistake the pause did not make, against a probe wasted on every stream known to be gone for a
// mistake it did.
func (s *scraper) probeCandidates() []string {
	monitored := s.monitoredStreams()
	candidates := make([]string, 0, len(monitored))

	for _, stream := range monitored {
		if !s.missing.firm(stream) {
			candidates = append(candidates, stream)
		}
	}

	// A fleet with nothing but firm exclusions has to ask something all the same, or the pause would
	// stand for good.
	if len(candidates) == 0 {
		return monitored
	}

	return candidates
}

// resumeUnprobed ends a pause whose probes were all rejected, so what the rotation kept landing on
// can be attributed to the streams that own it. The bisect it hands the session back to either finds
// those streams and excludes them, which is what lets the instances behind them recover, or is
// rejected everywhere and blames the group again for another round of probes.
//
// The bisect asks the whole fleet, whatever is excluded and whether or not a probe slot is due: a
// fallback is the one request that can settle the group's account, and every stream left out of it
// is a stream it cannot speak for. The exclusions made while the group was in doubt are released
// outright, since they were made on the group's account; a firm one is asked again but kept, so that
// a stream still gone is re-excluded without being counted or warned about a second time.
func (s *scraper) resumeUnprobed() {
	s.sweep = sweepUnderWay

	level.Info(s.logger).Log(
		"msg", "CloudWatch rejected every Enhanced Monitoring log group probe; isolating log streams instead.",
		"log_group", logGroupName,
		"probes", s.group.fallbackThreshold(),
	)

	s.retryTentative("CloudWatch log group probes were all rejected; retrying the log streams excluded while it was in doubt.")
}

// collectBatch collects the events of the given log streams. CloudWatch fails the whole request
// when any single stream does not exist, so the batch is halved until the missing streams are
// identified and excluded, which keeps the remaining instances reporting.
func (s *scraper) collectBatch(ctx context.Context, streams []string, sink *eventSink) error {
	err := s.collectPages(ctx, streams, sink)

	// An answered probe has already ended the pause by now, so a pause still standing here means the
	// probe was not answered. A rejection is swallowed rather than returned: it is the group's, not the
	// stream's the probe happened to name, and the ordinary attribution below would exclude that stream
	// for it. Any other failure is returned as it is, and the probe is asked again next scrape.
	if err != nil && s.group.paused() {
		if isResourceNotFound(err) {
			s.group.noteProbeRejected(time.Now())

			return nil
		}

		return err
	}

	if !isResourceNotFound(err) {
		return err
	}

	// Each batch gets its own budget, so a batch where every stream is missing cannot stop the
	// batches after it from finding and excluding theirs.
	s.evidence.isolationCalls = 0
	s.evidence.rejectedStreams += len(streams)

	return s.isolateMissing(ctx, streams, sink)
}

// beginAttribution starts the evidence this scrape will be read from, and takes up the sweep the last
// one asked for.
func (s *scraper) beginAttribution() {
	s.evidence.reset()

	if s.sweep == sweepRequested {
		s.sweep = sweepUnderWay
	} else {
		s.sweep = sweepNone
	}
}

// attributeRejections decides what the rejections this scrape collected were about: the log group,
// or the streams the scrape singled out. CloudWatch reports a missing log group and a missing log
// stream as the same error, so the two are told apart by what else the scrape was answered, and the
// charge is worth telling apart: one against the group pauses every instance in the session for a
// TTL, while one against a stream costs that stream an exclusion. An answer from anywhere settles
// it for the streams -- a group that answered is not what rejected them -- so it also ends the
// account the unanswered scrapes before it were keeping.
func (s *scraper) attributeRejections(mayBlameGroup bool) {
	if s.evidence.answered {
		clear(s.unansweredRejections)
		s.sweepCutShort = false
	}

	if !s.attributeToTheGroup(mayBlameGroup) {
		s.attributeToTheStreams()
	}

	s.keepTheBlamedGroupPaused()
}

// attributeToTheGroup charges the rejections to the log group when nothing in the scrape can account
// for them, and reports whether it did. A scrape that was answered nothing anywhere and singled out
// every stream it asked for is evidence about the group: reading it as streams instead would cost a
// bisect per scrape, exclude streams that exist, and name instances that are fine. Short of that the
// streams keep their own evidence, which is what the caller pays instead.
//
// The evidence has to span the scrape and not one batch of it: a missing group would have rejected
// the other batches too, so one batch of several saying nothing says nothing about the group, only
// that the instances in it are gone. Attributing every stream also has to stay within
// maxIsolationCalls, or the group could never be recognised for a batch of any size. A scrape with
// a request it was not answered for at all, because it ran out of time or was throttled or refused,
// therefore attributes its streams and leaves the group alone: holding the streams back as well
// would leave a bisect nothing to show for itself, and the next scrape would pay the same doomed
// bisect over the same batch for as long as whatever stopped this one lasts.
//
// The evidence has to span the fleet as well. A scrape that asked for the few streams a probe slot
// was due for, and was rejected everywhere, has heard nothing from the streams it held back, and
// those are exactly the ones that would answer if the group were fine: an exclusion is a stream that
// was gone, not one that still is. Blaming the group on that would pause every instance for a TTL
// over the streams known to be gone, and a fleet whose first maxProbesPerScrape exclusions stayed
// gone would be paused again every time their slots came due, while the streams behind them were
// never asked. The next scrape asks the whole fleet instead, and either the held-back streams
// answer or the rejection is finally the group's to take. The streams this scrape singled out are
// left as they are until then: excluding them now would exclude them on their own evidence when the
// group may be what rejected them, and the sweep asks them again either way.
//
// Spanning the fleet does not mean within one scrape. A sweep of a fleet that is all gone costs a
// full bisect, and the deadline cuts one that does not fit the interval at the same place every
// time; read scrape by scrape, the streams it reached would be excluded, the next scrape would be
// rejected over the rest and short of the fleet, and the sweep it asked for would be cut again, with
// the group never blamed and the bisect paid every other scrape for good. The rejections carried by
// the scrapes nothing answered therefore count towards the fleet alongside the present scrape's,
// while the exclusions an answered scrape made never do, since an answer is what makes a rejection
// the stream's own. Carried rejections are trusted only once a sweep has been cut, though. The first
// time they add up to the fleet, the scrape that singled them out may have been cut while the group
// was gone and the rest rejected after it came back, with the rest gone for real; the sweep asks
// both at once and settles that in one scrape when it fits, and a pause would cost the instances
// behind the first ones a TTL for a fault the group no longer has. A sweep that was cut cannot
// settle it, and the carried rejections are what is left.
func (s *scraper) attributeToTheGroup(mayBlameGroup bool) bool {
	rejectedEverywhere := mayBlameGroup && !s.evidence.answered && s.evidence.rejectedStreams >= minStreamsToBlameTheGroup &&
		len(s.evidence.isolated) == s.evidence.rejectedStreams
	if !rejectedEverywhere {
		return false
	}

	monitored := s.monitoredStreams()

	heldBack := s.streamsNotRejected(monitored)
	if heldBack == 0 && (s.evidence.rejectedStreams == len(monitored) || s.sweepCutShort) {
		s.markGroupMissing()

		return true
	}

	s.sweep = sweepRequested

	level.Info(s.logger).Log(
		"msg", "CloudWatch rejected every Enhanced Monitoring log stream requested; "+
			"asking the excluded ones too before blaming the log group.",
		"log_group", logGroupName,
		"log_streams_rejected", len(monitored)-heldBack,
		"log_streams_excluded", heldBack,
	)

	return true
}

// attributeToTheStreams excludes the streams this scrape singled out, on the terms the rest of the
// scrape earned them.
func (s *scraper) attributeToTheStreams() {
	// Whether the group's account was already open when this scrape began, which is to say whether
	// the scrape before it heard nothing either. Read before this scrape's own rejections join it.
	outage := len(s.unansweredRejections) > 0

	// A scrape nothing answered leaves the group's account open, so what it singled out is kept for
	// attributeToTheGroup to count once a later scrape is rejected over the rest of the fleet.
	if !s.evidence.answered {
		s.sweepCutShort = s.sweepCutShort || s.sweep == sweepUnderWay

		for _, stream := range s.evidence.isolated {
			s.unansweredRejections[stream] = struct{}{}
		}
	}

	// A scrape that was answered nowhere has the same gap in its evidence about each stream it singled
	// out: the rejection would have looked the same had the group been what rejected it. The stream is
	// excluded either way, since asking for it again would reject the whole batch again, but only
	// tentatively, so that the group answering releases it rather than leaving the instance behind it
	// waiting a TTL for a probe slot on evidence the group's return has just undermined.
	//
	// That doubt only exists while the group is a suspect: blamed, never heard from, or -- as here --
	// in the middle of an outage no scrape has heard the end of. A group that answered before and is
	// not blamed is not what rejected the streams a single unanswered scrape singled out, however the
	// rest of that scrape failed, and the next answer from it would release exclusions it had nothing
	// to do with: the streams would be requested again, rejected again and bisected again, every
	// other scrape, for as long as the healthy half kept being throttled. A run of scrapes that heard
	// nothing is the other case. Their rejections are being kept against the group precisely because
	// none of them could tell a fleet that is gone from a group that is, so the exclusions made
	// inside the run rest on the same open question, and reading them as the streams' own evidence
	// would warn by name about every instance of a fleet the group is about to be blamed for and then
	// hold them out of the sweep that ends the outage.
	tentative := !s.evidence.answered && (s.group.inDoubt() || outage)

	for _, stream := range s.evidence.isolated {
		s.markMissing(stream, tentative)
	}
}

// keepTheBlamedGroupPaused takes the pause back for a group still blamed at the end of a scrape
// nothing answered. A fallback gives the pause up so that the bisect it hands the session to can
// settle the group's account, and a bisect that ran out of time or was throttled settles nothing: the
// group is left blamed with no pause, which is a state nothing probes, so every scrape after it would
// pay the same bisect for as long as the outage lasted. Nothing the fallback heard bears on the blame
// it gave the pause up to test, so the pause stands again until something answers.
func (s *scraper) keepTheBlamedGroupPaused() {
	if s.sweep != sweepUnderWay || s.evidence.answered {
		return
	}

	s.group.resumeAfterFallback(time.Now(), s.currentProbeSpacing())
}

// streamsNotRejected counts the monitored streams neither this scrape nor the unanswered scrapes
// before it were rejected over, which is what still stands between the rejection and the group.
func (s *scraper) streamsNotRejected(monitored []string) int {
	rejected := make(map[string]struct{}, len(s.evidence.isolated)+len(s.unansweredRejections))
	for _, stream := range s.evidence.isolated {
		rejected[stream] = struct{}{}
	}

	for stream := range s.unansweredRejections {
		rejected[stream] = struct{}{}
	}

	heldBack := 0

	for _, stream := range monitored {
		if _, ok := rejected[stream]; !ok {
			heldBack++
		}
	}

	return heldBack
}

// isolateMissing halves a rejected batch until it can attribute the rejection to single log
// streams, spending at most maxIsolationCalls requests per scrape. What it cannot attribute this
// time is retried on the next scrape with a fresh budget.
func (s *scraper) isolateMissing(ctx context.Context, streams []string, sink *eventSink) error {
	if len(streams) == 1 {
		s.evidence.isolated = append(s.evidence.isolated, streams[0])

		return nil
	}

	mid := len(streams) / bisectDivisor

	return errors.Join(
		s.isolateHalf(ctx, streams[:mid], sink),
		s.isolateHalf(ctx, streams[mid:], sink),
	)
}

func (s *scraper) isolateHalf(ctx context.Context, streams []string, sink *eventSink) error {
	if s.evidence.isolationCalls >= maxIsolationCalls {
		return errIsolationBudget
	}

	s.evidence.isolationCalls++

	err := s.collectPages(ctx, streams, sink)
	if !isResourceNotFound(err) {
		return err
	}

	return s.isolateMissing(ctx, streams, sink)
}

// markMissing excludes a log stream from later requests, tentatively when the rejection may as well
// be the log group's: nothing this scrape asked was answered, and the group is blamed or has never
// answered. It counts and logs only what changed, so that a permanently missing stream neither
// inflates the counter nor floods the log every scrape.
func (s *scraper) markMissing(logStreamName string, tentative bool) {
	outcome := s.missing.mark(logStreamName, time.Now(), tentative)
	if outcome == markUnchanged {
		return
	}

	// A stream confirmed gone was counted when it was first excluded; only its evidence changed.
	if outcome != markConfirmedFirm {
		s.errorCounts[errorKindNotFound]++
	}

	keyvals := []any{
		"msg", "CloudWatch log stream does not exist; excluding it from Enhanced Monitoring requests.",
		"log_stream", logStreamName,
		"instance", s.instanceNameFor(logStreamName),
	}

	// While the group is in doubt a stream singled out is not news of its own. A fallback bisect of a
	// group that is still gone is rejected everywhere, and one the deadline cuts short singles out
	// every stream it reached without ever being able to say so of the group; each would otherwise be
	// a warning naming an instance that is fine. The warning is kept for an exclusion that rests on
	// evidence about the stream itself.
	if outcome == markNewTentative {
		level.Info(s.logger).Log(keyvals...)

		return
	}

	level.Warn(s.logger).Log(keyvals...)
}

// markGroupMissing pauses the requests of a session whose log group does not exist. Every instance
// in it is then waiting on the same thing rather than on a stream of its own.
//
// Like markMissing it reports and logs only the transition, so that a group which stays missing
// neither inflates the counter nor repeats the warning. That is not automatic here: a fallback
// gives the pause up in order to test it, and so brings the session back past this every time it
// finds nothing. Counting those would turn one outage into a rate.
func (s *scraper) markGroupMissing() {
	clear(s.unansweredRejections)
	s.sweepCutShort = false

	if !s.group.blame(time.Now(), s.currentProbeSpacing()) {
		return
	}

	s.errorCounts[errorKindGroupNotFound]++

	level.Warn(s.logger).Log(
		"msg", "CloudWatch log group does not exist; pausing Enhanced Monitoring requests.",
		"log_group", logGroupName,
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

		// A later page failing must not hold a stream out of the next request for another TTL: the
		// request was already answered once, which is all the evidence its streams exist.
		s.noteAnswered(streams)

		for _, event := range output.Events {
			s.handleEvent(event, sink)
		}
	}

	return nil
}

// noteAnswered records everything an answered request proves: the log group exists, every stream it
// listed exists, and the batch being bisected has at least one half that is not the problem.
//
// A group cleared of blame has the next scrape ask the whole fleet at once. The exclusions still
// standing were all made or renewed while the group was gone, whatever evidence they rest on: a firm
// one made before the group was blamed was renewed by every rejected probe and fallback since, and
// a fleet coming back would otherwise return maxProbesPerScrape streams per scrape, in whatever order
// their slots came due, for a fault that has just been explained away. A fallback that is answered
// has asked the fleet already, and the streams it is rejected over are excluded on their own evidence
// by the end of it; sweeping again would only bisect them a second time.
func (s *scraper) noteAnswered(streams []string) {
	s.evidence.answered = true

	if s.group.noteAnswered() {
		msg := "CloudWatch log group exists again; requesting every Enhanced Monitoring log stream."
		if s.sweep == sweepUnderWay {
			msg = "CloudWatch log group exists again; resuming Enhanced Monitoring requests."
		} else {
			s.sweep = sweepRequested
		}

		level.Info(s.logger).Log("msg", msg, "log_group", logGroupName)
	}

	s.clearAccepted(streams)
	s.retryTentative("CloudWatch log group answered; retrying the log streams excluded while it was in doubt.")
}

// retryTentative stops excluding the streams a scrape answered nowhere had singled out, now that the
// group has answered and can no longer be what rejected them. They are requested again on the next
// scrape: the ones that exist report, and one that is gone for a reason of its own is rejected again
// and excluded on evidence about itself. Waiting for their probes instead would hold every instance
// behind them back for a TTL and then let them return maxProbesPerScrape at a time, for a fault the
// group's return has just explained away.
func (s *scraper) retryTentative(msg string) {
	released := s.missing.releaseTentative()
	if released == 0 {
		return
	}

	level.Info(s.logger).Log("msg", msg, "log_group", logGroupName, "log_streams", released)
}

// clearAccepted stops excluding the log streams of a page CloudWatch answered. A rejection names
// no stream, so answering the request is the only positive evidence that every stream listed in it
// exists. Waiting for an event instead would keep a stream that exists but published nothing inside
// the request window excluded for another TTL, and since the window is only as wide as the fastest
// instance's reporting interval, that is the common case rather than the exception.
func (s *scraper) clearAccepted(streams []string) {
	for _, stream := range streams {
		if !s.missing.clear(stream) {
			continue
		}

		level.Info(s.logger).Log(
			"msg", "CloudWatch log stream exists again; resuming Enhanced Monitoring requests.",
			"log_stream", stream,
			"instance", s.instanceNameFor(stream),
		)
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

	timestamp := time.UnixMilli(aws.ToInt64(event.Timestamp)).UTC()
	s.reportClockSkew(timestamp, logger)

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

		sink.add(keyOf(s.session, instance), timestamp, osMetrics.makePrometheusMetrics(instance.Region, instance.Labels))
	}
}

// reportClockSkew counts an event timestamped further ahead than the exporter's clock explains.
// CloudWatch accepts log events dated up to two hours ahead, so the timestamp says as much about the
// two clocks as about the event. The sample is exported either way, which is why the count is not an
// error count: the request window and the expiry are the parts a future timestamp could stall, and
// both clamp it to now themselves. One event is logged at debug because a host whose clock drifts
// produces one per instance per scrape; scrape reports the total once.
func (s *scraper) reportClockSkew(timestamp time.Time, logger log.Logger) {
	if !timestamp.After(time.Now().Add(clockSkewReportThreshold)) {
		return
	}

	s.skewedEvents++

	level.Debug(logger).Log("msg", "Enhanced Monitoring event is timestamped in the future.")
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

	err := s.updateInstanceStates(ctx)
	s.scheduleNextRefresh(err != nil)

	return err
}

// scheduleNextRefresh decides when the instance states are worth asking AWS for again. A failed
// refresh leaves every instance its paginator never reached with the resource ID it already had, so
// waiting the full interval keeps a switchover this scraper cannot see from a retired log stream that
// the isolation is meanwhile about to exclude as missing. The retry therefore backs off between the
// scrape interval and the refresh interval: soon enough that one throttled page costs a scrape or
// two, bounded so that a DescribeDBInstances that keeps failing is not asked once per scrape.
func (s *scraper) scheduleNextRefresh(failed bool) {
	if !failed {
		s.refreshBackoff = 0
		s.nextResourceIDRefresh = time.Now().Add(resourceIDRefreshInterval).Round(0)

		return
	}

	s.refreshBackoff = min(max(refreshBackoffFactor*s.refreshBackoff, s.interval()), resourceIDRefreshInterval)
	s.nextResourceIDRefresh = time.Now().Add(s.refreshBackoff).Round(0)
}

// updateInstanceStates follows the resource ID and the Enhanced Monitoring interval AWS reports.
// InstanceStates returns the pages it did read alongside its error, and a partial result is still
// authoritative for the instances it does contain: waiting a whole refresh interval for a resource ID
// this scraper could already see would leave a retired log stream to be excluded as missing, which is
// the outcome the isolation exists to prevent.
func (s *scraper) updateInstanceStates(ctx context.Context) error {
	states, err := s.stateResolver.InstanceStates(ctx)

	for instanceIndex, instance := range s.instances {
		state, ok := states[instance.Instance]
		if !ok || state.ResourceID == "" {
			s.logMissingResourceID(instance, err != nil)

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

	if err != nil {
		return fmt.Errorf("failed to refresh instance states: %w", err)
	}

	return nil
}

// logMissingResourceID reports an instance AWS returned no resource ID for. When the refresh failed,
// every instance the paginator never reached looks the same as one that is genuinely gone, so the
// report is demoted rather than filling the log with a line per instance on every throttle.
func (s *scraper) logMissingResourceID(instance sessions.Instance, refreshFailed bool) {
	keyvals := []any{
		"msg", "RDS resource ID not found.",
		"region", instance.Region,
		"instance", instance.Instance,
	}

	if refreshFailed {
		level.Debug(s.logger).Log(keyvals...)

		return
	}

	level.Warn(s.logger).Log(keyvals...)
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

// newestEventTimes returns the event timestamp to judge each instance by, the oldest of those
// timestamps, and whether any events were collected at all. The oldest is where the next request has
// to start, and when nothing was collected the caller keeps its current start time.
func newestEventTimes(allTimes map[instanceKey][]time.Time, now time.Time) (map[instanceKey]time.Time, time.Time, bool) {
	times := make(map[instanceKey]time.Time, len(allTimes))

	var oldestNewest time.Time

	for key, events := range allTimes {
		newest, collected := newestEventTime(events, now)
		if !collected {
			continue
		}

		times[key] = newest

		if oldestNewest.IsZero() || oldestNewest.After(newest) {
			oldestNewest = newest
		}
	}

	return times, oldestNewest, len(times) > 0
}

// newestEventTime returns the newest event the exporter's clock accounts for, falling back to the
// newest of all of them when it accounts for none. An event dated up to clockSkewReportThreshold
// ahead counts as current: a host a few seconds behind AWS sees every latest event dated ahead, and
// preferring a strictly past one would judge each instance by the sample before its latest on every
// scrape, without the skew ever being reported. Beyond the threshold the timestamp says more about
// the clocks than the event, and the collector compares raw timestamps to tell one sample from the
// next, so an event dated that far ahead of the real ones would sit in front of them until now caught
// up, and the instance would publish nothing meanwhile. The fallback is what a host well behind AWS
// relies on: when every event is dated ahead there is nothing else to judge the instance by, and no
// skew may cost it its sample.
func newestEventTime(events []time.Time, now time.Time) (time.Time, bool) {
	accepted := now.Add(clockSkewReportThreshold)

	var newest, newestAccepted time.Time

	for _, timestamp := range events {
		if newest.Before(timestamp) {
			newest = timestamp
		}

		if !timestamp.After(accepted) && newestAccepted.Before(timestamp) {
			newestAccepted = timestamp
		}
	}

	if !newestAccepted.IsZero() {
		return newestAccepted, true
	}

	return newest, !newest.IsZero()
}
