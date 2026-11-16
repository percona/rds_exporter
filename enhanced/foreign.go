package enhanced

import (
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	"github.com/percona/rds_exporter/sessions"
)

// maxLoggedPayloadName caps how much of the instance name an event carries is logged.
const maxLoggedPayloadName = 64

// maxEarlyRefreshes is how many refreshes a foreign event episode asks for before it waits for the
// regular refresh.
const maxEarlyRefreshes = 3

// episodeStage is how far the early refresh of a foreign event episode has got.
type episodeStage int

const (
	// episodeOpen waits for its early refreshes.
	episodeOpen episodeStage = iota
	// episodeAsked has had maxEarlyRefreshes refreshes that could not settle it, and waits for the
	// regular refresh.
	episodeAsked
	// episodeConfirmed has had a refresh return the same resource ID for the configured name after
	// maxEarlyRefreshes, so AWS and the events disagree on what the stream belongs to.
	episodeConfirmed
)

// foreignEpisode is a run of events naming another instance than the configured one, on one log
// stream and under one name.
type foreignEpisode struct {
	resourceID  string
	payloadName string
	newest      time.Time
	ownLast     bool
	refreshes   int
	counted     time.Time
	stage       episodeStage
}

// claimEvent reports whether an event is the configured instance's own, and accounts for it as a
// foreign event when it is not.
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
		refreshes:   0,
		counted:     time.Time{},
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

// firstForeignRead records a foreign event and reports whether it had not been read before, taking an
// event without an ID as new every time.
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

// forgetForeignReads drops the foreign events dated before the millisecond the request window starts in.
func (s *scraper) forgetForeignReads(start time.Time) {
	requested := time.UnixMilli(start.UnixMilli())

	for key, reads := range s.foreignReads {
		for eventID, timestamp := range reads {
			if timestamp.Before(requested) {
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

// markForeignEpisodesAsked counts a refresh for every waiting episode not counted within
// earlyRefreshSpacing, and leaves one that reaches maxEarlyRefreshes to the regular refresh.
func (s *scraper) markForeignEpisodesAsked(now time.Time) {
	for key, episode := range s.foreignEpisodes {
		if !episode.wantsRefresh() || now.Before(episode.counted.Add(earlyRefreshSpacing)) {
			continue
		}

		episode.counted = now.Round(0)
		episode.refreshes++

		if episode.refreshes >= maxEarlyRefreshes {
			episode.stage = episodeAsked
		}

		s.foreignEpisodes[key] = episode
	}
}

// settleForeignEpisode ends a refreshed instance's episode on a new resource ID, and reports it once
// when the same ID comes back after maxEarlyRefreshes while another name still comes last.
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

	if episode.ownLast || episode.stage != episodeAsked {
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
