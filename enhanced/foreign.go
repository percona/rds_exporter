package enhanced

import (
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	"github.com/percona/rds_exporter/sessions"
)

// maxLoggedPayloadName caps how much of the instance name an event carries is logged. The name is
// AWS input and is never exported.
const maxLoggedPayloadName = 64

// foreignEpisode is a run of events naming another instance than the configured one, on one log
// stream and under one name. The first event of an episode asks for the instance states to be
// refreshed early, since a blue/green switchover is the usual cause and the refresh is what repoints
// the instance; asked keeps a refresh that could not settle the episode from being asked for again
// on every scrape. confirmed is a refresh having returned the same resource ID, which leaves a rename
// in AWS or a stale configuration as the cause, and nothing an early refresh can fix.
type foreignEpisode struct {
	resourceID  string
	payloadName string
	asked       bool
	confirmed   bool
}

// ownsEvent reports whether an event is the configured instance's own. The log stream alone does not
// say so: after a blue/green switchover the stream is read until the next refresh repoints it, while
// AWS has already renamed the box behind it, and the lookback after the repoint re-reads the promoted
// box's events from before it took the name. Neither describes the instance PMM was configured with.
// The request still counts as answered, so a foreign event never reads as a missing stream.
func (s *scraper) ownsEvent(instance sessions.Instance, payloadName string, logger log.Logger) bool {
	key := keyOf(s.session, instance)

	if payloadName == instance.Instance {
		delete(s.foreign, key)

		return true
	}

	s.foreignEvents++
	loggedName := truncate(payloadName, maxLoggedPayloadName)

	//nolint:errcheck // a log line that cannot be written has nowhere to be reported, as elsewhere here.
	level.Debug(logger).Log("msg", "Enhanced Monitoring event names another instance; dropped.",
		"payload_instance", loggedName)

	episode, known := s.foreign[key]
	if known && episode.resourceID == instance.ResourceID && episode.payloadName == payloadName {
		return false
	}

	s.foreign[key] = foreignEpisode{
		resourceID: instance.ResourceID, payloadName: payloadName, asked: false, confirmed: false,
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

// foreignRefreshWanted reports whether an episode is still waiting for its early refresh.
func (s *scraper) foreignRefreshWanted() bool {
	for _, episode := range s.foreign {
		if !episode.asked {
			return true
		}
	}

	return false
}

// markForeignEpisodesAsked records that a refresh has run for every episode open at its start,
// whatever it returns: an instance the refresh did not return or could not read is left to the
// regular refresh.
func (s *scraper) markForeignEpisodesAsked() {
	for key, episode := range s.foreign {
		if episode.asked {
			continue
		}

		episode.asked = true
		s.foreign[key] = episode
	}
}

// settleForeignEpisode closes the episode of an instance a refresh has returned the state of. A new
// resource ID ends it; the same one confirms that the instance is named differently in AWS than in
// PMM, which is reported once.
func (s *scraper) settleForeignEpisode(instance sessions.Instance) {
	key := keyOf(s.session, instance)

	episode, known := s.foreign[key]
	if !known {
		return
	}

	if episode.resourceID != instance.ResourceID {
		delete(s.foreign, key)

		return
	}

	if episode.confirmed {
		return
	}

	episode.confirmed = true
	s.foreign[key] = episode

	//nolint:errcheck // a log line that cannot be written has nowhere to be reported, as elsewhere here.
	level.Warn(s.logger).Log(
		"msg", "Enhanced Monitoring events name another instance than the one configured for their log stream; "+
			"it may have been renamed in AWS without PMM being updated.",
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
