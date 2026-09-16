package enhanced

import "time"

const (
	// minStreamsToBlameTheGroup is how many log streams a rejected batch needs before its rejection
	// can be read as evidence about the log group. A request for one stream is rejected the same way
	// whether the stream or the group is what does not exist, so one stream can only blame itself.
	// Two are barely better: a pair of instances leaving CloudWatch at once is ordinary fleet churn,
	// and a session monitoring only that pair cannot tell it from the group disappearing. The misread
	// is expensive in one direction only -- blaming the group pauses every instance in the session for
	// a TTL, while blaming the streams costs an exclusion each and clears itself on the next probe --
	// so a session that small attributes its streams instead.
	minStreamsToBlameTheGroup = 3

	// maxRejectedProbes is how many unanswered probes a log group that has answered before is given
	// before the session goes back to isolating its streams. A rejected probe cannot say whether the group
	// or the stream it named is what does not exist, so a rotation that keeps landing on streams
	// that are genuinely gone would hold the whole session back one TTL at a time for as long as
	// they stay gone. Three keeps the cheap explanation cheap -- a group that really is missing is
	// asked for one stream three times before it costs a bisect -- while capping what the expensive
	// one can cost the instances that are fine.
	maxRejectedProbes = 3

	// maxProbeBackoff caps how far the wait between fallbacks is allowed to double. A fallback is
	// worth its bisect early and worth much less late: the probe rotation recovers a wrongly blamed
	// group on its own as soon as it lands on a stream that answers, so what a fallback buys is
	// arriving there sooner, and buying that again every fifteen minutes spends the fleet's whole
	// request budget on an answer that has not changed since the last one. What the cap sets is how
	// long a session can take to notice a fleet coming back after everything in it stayed gone: the
	// fallback is the only thing that asks the whole fleet at once, so nothing else would find the
	// first stream to return until the rotation reached it. Three doublings stretch the wait from
	// three pauses to twenty-four, which is two hours at missingStreamTTL, and leave the cost of
	// staying ready one bisect per two hours rather than one per fifteen minutes.
	maxProbeBackoff = 3
)

// logGroup is what a session knows about its RDSOSMetrics log group: whether it has ever answered,
// whether it is blamed for the rejections at hand, and the pause and the probes that blame costs.
// It is only used from the scrape goroutine, so it needs no lock, and it holds state only: each
// method reports the transition it made, and the scraper counts and logs it.
type logGroup struct {
	// probeAfter is when the next probe of a paused session is due. Zero while requests are not
	// paused, which is the ordinary state.
	probeAfter time.Time
	// probes is the rotation cursor. It is deliberately never rewound, so that the probes following
	// a fallback ask streams the earlier ones did not.
	probes int
	// rejectedProbes counts the probes of the current pause rejected over existence. Only those count
	// towards abandoning it: a throttled or refused probe says nothing about the group, and counting
	// it would let rate limiting alone talk the session into bisecting the whole fleet.
	rejectedProbes int
	// unproductiveFallbacks counts the fallbacks of the current outage that found nothing, which is
	// the one thing that says the next fallback is not worth what the last one cost.
	unproductiveFallbacks int
	// seen is whether the group answered at least once in the life of the session. A group that has
	// answered and then goes dark has changed, and that is what makes giving up on a pause worth a
	// bisect; one that never answered is most likely a region that never enabled Enhanced Monitoring.
	seen bool
	// blamed is whether the group is held responsible for the rejections at hand. It stands until
	// the group answers, whatever the pause is doing, because a fallback holds the blame with no
	// pause left to clear.
	blamed bool
}

// probeDecision is what a paused session is to request.
type probeDecision int

const (
	probeNotPaused probeDecision = iota
	probeWaiting
	probeDue
	probeGivenUp
)

// paused reports whether requests are held back while the group is presumed missing.
func (g *logGroup) paused() bool {
	return !g.probeAfter.IsZero()
}

// inDoubt reports whether a rejection nothing else in the scrape answered for may be the group's
// doing: it is blamed, or it has never answered.
func (g *logGroup) inDoubt() bool {
	return g.blamed || !g.seen
}

// fallbackThreshold is how many rejected probes the current pause is given before it is abandoned
// for a bisect. It doubles for each fallback of the outage that found nothing, up to maxProbeBackoff.
func (g *logGroup) fallbackThreshold() int {
	return maxRejectedProbes << g.unproductiveFallbacks
}

// probe decides what a paused session requests: nothing until the probe is due, then the next stream
// of the rotation. Which stream is asked rotates, because a probe rejected while the group is presumed
// missing is credited to the group and teaches nothing about the stream it named; asking the same one
// every time is unrecoverable once that stream is the only thing still gone. Rotating is not enough
// on its own, because every stream it lands on while they are all still gone buys the pause another
// TTL, so a pause the group has earned by going dark is given up after fallbackThreshold rejected
// probes, and the streams are isolated the ordinary way. A group that has never answered is left to
// its probes instead: bisecting a fleet for a region that never enabled Enhanced Monitoring would pay
// the full cost of the answer a single probe already has.
func (g *logGroup) probe(streams []string, now time.Time) (string, probeDecision) {
	if !g.paused() || len(streams) == 0 {
		return "", probeNotPaused
	}

	if now.Before(g.probeAfter) {
		return "", probeWaiting
	}

	if g.seen && g.rejectedProbes >= g.fallbackThreshold() {
		g.probeAfter = time.Time{}

		return "", probeGivenUp
	}

	stream := streams[g.probes%len(streams)]
	g.probes++

	return stream, probeDue
}

// blame pauses requests for a TTL and reports whether that is news. Blame the session already held
// is not: reaching here twice means a fallback bisected the fleet and found nothing that answers.
// Only the probes of the new pause count towards abandoning it.
func (g *logGroup) blame(now time.Time) bool {
	g.probeAfter = now.Add(missingStreamTTL)
	g.rejectedProbes = 0

	if g.blamed {
		g.unproductiveFallbacks = min(g.unproductiveFallbacks+1, maxProbeBackoff)

		return false
	}

	g.blamed = true

	return true
}

// noteProbeFailed extends the pause by a TTL after a probe that was not answered, which has spent its
// turn whatever the reason, and counts it against the pause only if it was rejected over existence.
func (g *logGroup) noteProbeFailed(rejected bool, now time.Time) {
	g.probeAfter = now.Add(missingStreamTTL)

	if rejected {
		g.rejectedProbes++
	}
}

// noteAnswered records that the group exists and is not the suspect any more, so that the next time
// it is blamed is a new outage whose first fallback is worth paying for again. It reports whether
// that ended a pause.
func (g *logGroup) noteAnswered() bool {
	g.seen = true
	g.blamed = false
	g.unproductiveFallbacks = 0

	resumed := g.paused()
	g.probeAfter = time.Time{}

	return resumed
}
