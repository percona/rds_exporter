package enhanced

import (
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	"github.com/percona/rds_exporter/sessions"
)

// maxLoggedPayloadName caps how much of the instance name an event carries reaches the log. The name
// is AWS input and only ever logged, never exported.
const maxLoggedPayloadName = 64

// ownsEvent reports whether an event is the configured instance's own. The log stream alone does not
// say so: after a blue/green switchover the stream is read until the next refresh repoints it, while
// AWS has already renamed the box behind it, and the lookback after the repoint re-reads the promoted
// box's events from before it took the name. Neither describes the instance PMM was configured with.
// The request still counts as answered, so a foreign event never reads as a missing stream.
func (s *scraper) ownsEvent(instance sessions.Instance, payloadName string, logger log.Logger) bool {
	if payloadName == instance.Instance {
		return true
	}

	s.foreignEvents++

	//nolint:errcheck // a log line that cannot be written has nowhere to be reported, as elsewhere here.
	level.Debug(logger).Log("msg", "Enhanced Monitoring event names another instance; dropped.",
		"payload_instance", truncate(payloadName, maxLoggedPayloadName))

	return false
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
