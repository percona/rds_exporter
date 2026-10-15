package enhanced

import (
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	"github.com/percona/rds_exporter/sessions"
)

// maxLoggedPayloadName caps how much of the instance name an event carries is logged. The name is
// AWS input and is never exported.
const maxLoggedPayloadName = 64

// episodeStage is how far the early refresh of a foreign event episode has got.
type episodeStage int

const (
	// episodeOpen waits for its early refresh.
	episodeOpen episodeStage = iota
	// episodeAsked has had a refresh that could not settle it, and is not asked for again on every
	// scrape.
	episodeAsked
	// episodeConfirmed has had a refresh return the same resource ID for the configured name, so AWS
	// and the events disagree on what the stream belongs to, and no early refresh can fix that.
	episodeConfirmed
)

// foreignEpisode is a run of events naming another instance than the configured one, on one log
// stream and under one name. The first event of an episode asks for the instance states to be
// refreshed early, since a blue/green switchover is the usual cause and the refresh is what repoints
// the instance. ownLast is the instance's own event having come after the episode's newest one, as
// when the lookback after a repoint re-reads the promoted box's events from before it took the name;
// the stream reports the instance after all, so it is neither refreshed for nor reported.
type foreignEpisode struct {
	resourceID  string
	payloadName string
	newest      time.Time
	ownLast     bool
	stage       episodeStage
}

// claimEvent reports whether an event is the configured instance's own, and accounts for it when it
// is not. The log stream alone does not say so: after a blue/green switchover the stream is read until
// the next refresh repoints it, while AWS has already renamed the box behind it. The request still
// counts as answered, so a foreign event never reads as a missing stream.
//
// A dropped event does not move the request window, so it is read again until the window passes it;
// it is counted and logged the first time only. Only an event newer than the episode's newest moves
// the episode on: CloudWatch can ingest an event after a newer one, and the order the stream reports
// the names in is what the episode is about.
func (s *scraper) claimEvent(
	instance sessions.Instance, payloadName, eventID string, timestamp time.Time, logger log.Logger,
) bool {
	key := keyOf(s.session, instance)

	episode, known := s.foreignEpisodes[key]
	known = known && episode.resourceID == instance.ResourceID
	newer := !known || timestamp.After(episode.newest)

	if payloadName == instance.Instance {
		if known && newer {
			episode.ownLast = true
			s.foreignEpisodes[key] = episode
		}

		return true
	}

	if !s.firstForeignRead(key, eventID, timestamp) {
		return false
	}

	s.counts.foreign++
	loggedName := truncate(payloadName, maxLoggedPayloadName)

	//nolint:errcheck // a log line that cannot be written has nowhere to be reported, as elsewhere here.
	level.Debug(logger).Log("msg", "Enhanced Monitoring event names another instance; dropped.",
		"payload_instance", loggedName)

	if !newer {
		return false
	}

	// An event dated beyond the skew threshold says more about the clocks than about the order of the
	// events, and taken at its word it would hold the episode until the clock caught up with it.
	newest := notAfter(timestamp, time.Now().Add(clockSkewReportThreshold))

	if known && episode.payloadName == payloadName {
		episode.newest = newest
		episode.ownLast = false
		s.foreignEpisodes[key] = episode

		return false
	}

	s.foreignEpisodes[key] = foreignEpisode{
		resourceID:  instance.ResourceID,
		payloadName: payloadName,
		newest:      newest,
		ownLast:     false,
		stage:       episodeOpen,
	}

	//nolint:errcheck // a log line that cannot be written has nowhere to be reported, as elsewhere here.
	level.Info(logger).Log(
		"msg", "Enhanced Monitoring events name another instance than the one configured for their log stream; "+
			"dropping them.",
		"resource_id", instance.ResourceID,
		"payload_instance", loggedName,
	)

	return false
}

// firstForeignRead records a foreign event and reports whether it had not been read before. An event
// without an ID cannot be told apart from another, so it is taken as new every time.
func (s *scraper) firstForeignRead(key instanceKey, eventID string, timestamp time.Time) bool {
	if eventID == "" {
		return true
	}

	reads, ok := s.foreignReads[key]
	if !ok {
		reads = make(map[string]time.Time)
		s.foreignReads[key] = reads
	}

	if _, read := reads[eventID]; read {
		return false
	}

	reads[eventID] = timestamp

	return true
}

// forgetForeignReads drops the foreign events the request window starting at start can no longer
// return. FilterLogEvents treats the start as inclusive, so an event dated exactly at it is kept.
func (s *scraper) forgetForeignReads(start time.Time) {
	for key, reads := range s.foreignReads {
		for eventID, timestamp := range reads {
			if timestamp.Before(start) {
				delete(reads, eventID)
			}
		}

		if len(reads) == 0 {
			delete(s.foreignReads, key)
		}
	}
}

// wantsRefresh reports whether the episode still waits for its early refresh.
func (e foreignEpisode) wantsRefresh() bool {
	return e.stage == episodeOpen && !e.ownLast
}

// foreignRefreshWanted reports whether an episode is still waiting for its early refresh.
func (s *scraper) foreignRefreshWanted() bool {
	for _, episode := range s.foreignEpisodes {
		if episode.wantsRefresh() {
			return true
		}
	}

	return false
}

// markForeignEpisodesAsked records that a refresh has run for every episode waiting at its start,
// whatever it returns: an instance the refresh did not return or could not read is left to the
// regular refresh.
func (s *scraper) markForeignEpisodesAsked() {
	for key, episode := range s.foreignEpisodes {
		if !episode.wantsRefresh() {
			continue
		}

		episode.stage = episodeAsked
		s.foreignEpisodes[key] = episode
	}
}

// settleForeignEpisode closes the episode of an instance a refresh has returned the state of. A new
// resource ID ends it. The same one, while the stream's newest events still name another instance,
// confirms that AWS and the events disagree, which is reported once.
func (s *scraper) settleForeignEpisode(instance sessions.Instance) {
	key := keyOf(s.session, instance)

	episode, known := s.foreignEpisodes[key]
	if !known {
		return
	}

	if episode.resourceID != instance.ResourceID {
		delete(s.foreignEpisodes, key)

		return
	}

	if episode.ownLast || episode.stage == episodeConfirmed {
		return
	}

	episode.stage = episodeConfirmed
	s.foreignEpisodes[key] = episode

	//nolint:errcheck // a log line that cannot be written has nowhere to be reported, as elsewhere here.
	level.Warn(s.logger).Log(
		"msg", "AWS reports this log stream for the configured instance, but its Enhanced Monitoring events "+
			"name another instance; they are dropped.",
		"region", instance.Region,
		"instance", instance.Instance,
		"resource_id", instance.ResourceID,
		"payload_instance", truncate(episode.payloadName, maxLoggedPayloadName),
	)
}

// truncate returns at most limit characters of value, never splitting one.
func truncate(value string, limit int) string {
	count := 0

	for i := range value {
		if count == limit {
			return value[:i]
		}

		count++
	}

	return value
}
