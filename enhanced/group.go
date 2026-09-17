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

	// minProbesBeforeFallback is the fewest rejected probes a blamed log group is given before the
	// session goes back to isolating its streams. The pause is otherwise given up after a TTL's worth
	// of them, however many the spacing fits in one, but a fleet of one or two streams is probed no
	// more often than once a TTL, and giving it up after one rejection would hand a bisect the same
	// answer the probe had. Three keeps the cheap explanation cheap for the smallest fleets -- a group
	// that really is missing is asked for one stream three times before it costs a bisect -- while
	// capping what the expensive one can cost the instances that are fine.
	minProbesBeforeFallback = 3

	// maxProbeBackoff caps how far the wait between fallbacks is allowed to double. A fallback is
	// worth its bisect early and worth much less late: the probe rotation recovers a wrongly blamed
	// group on its own as soon as it lands on a stream that answers, so what a fallback buys is
	// arriving there sooner, and buying that again every TTL spends the fleet's whole request budget
	// on an answer that has not changed since the last one. What the cap sets is how long a session
	// can take to notice a fleet coming back after everything in it stayed gone: the fallback is the
	// only thing that asks the whole fleet at once, so nothing else would find the first stream to
	// return until the rotation reached it. Five doublings stretch the wait from one TTL to
	// thirty-two, which is two hours and forty minutes at missingStreamTTL, and leave the cost of
	// staying ready one bisect per that rather than one per five minutes.
	maxProbeBackoff = 5
)

// logGroup is what a session knows about its RDSOSMetrics log group: whether it has ever answered,
// whether it is blamed for the rejections at hand, and the pause and the probes that blame costs.
// It is only used from the scrape goroutine, so it needs no lock, and it holds state only: each
// method reports the transition it made, and the scraper counts and logs it.
type logGroup struct {
	// probeAfter is when the next probe of a paused session is due. Zero while requests are not
	// paused, which is the ordinary state.
	probeAfter time.Time
	// spacing is the wait between the probes of the current pause. The scraper sets it from the
	// fleet, so that the rotation covers every stream the group could be blamed for within one TTL
	// when the scrape interval allows, and asks one stream per scrape when it does not.
	spacing time.Duration
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
	// seen is whether the group answered at least once in the life of the session. A group that
	// never answered is most likely a region that never enabled Enhanced Monitoring, and until it
	// answers it stays a suspect for every rejection nothing else in a scrape answered for.
	seen bool
	// blamed is whether the group is held responsible for the rejections at hand. It stands until
	// the group answers, whatever the pause is doing, because a fallback holds the blame with no
	// pause left to clear.
	blamed bool
}

func newLogGroup() logGroup {
	return logGroup{
		probeAfter:            time.Time{},
		spacing:               0,
		probes:                0,
		rejectedProbes:        0,
		unproductiveFallbacks: 0,
		seen:                  false,
		blamed:                false,
	}
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
// for a bisect: a TTL's worth at the current spacing, and never fewer than minProbesBeforeFallback.
// It doubles for each fallback of the outage that found nothing, up to maxProbeBackoff.
func (g *logGroup) fallbackThreshold() int {
	probesPerTTL := 1
	if g.spacing > 0 {
		probesPerTTL = int(missingStreamTTL / g.spacing)
	}

	return max(minProbesBeforeFallback, probesPerTTL) << g.unproductiveFallbacks
}

// probe decides what a paused session requests: nothing until the probe is due, then the next stream
// of the rotation. Which stream is asked rotates, because a probe rejected while the group is presumed
// missing is credited to the group and teaches nothing about the stream it named; asking the same one
// every time is unrecoverable once that stream is the only thing still gone. The probes are spaced
// so that the rotation goes round the fleet once per TTL where the scrape interval allows: a fleet
// blamed together because its streams were not created yet has each of them asked within a TTL of
// existing, where one probe per TTL would have held the last of them back a TTL per stream ahead of
// it. Rotating is not enough on its own, because every stream it lands on while they are all still
// gone buys the pause more silence, so the pause is given up after fallbackThreshold rejected probes,
// and the streams are isolated the ordinary way. A group that has never answered falls back like any
// other: a region that never enabled Enhanced Monitoring pays a bisect for the answer a probe already
// had, but the backoff makes that one bisect per few hours at most, whereas a fleet too large for
// its interval to be gone round in a TTL would otherwise wait a scrape per stream ahead of the one
// that exists for that one instance to report. A session with no stream to name waits as if the
// probe were not due: it has nothing to ask, and giving up would hand a bisect nothing either.
func (g *logGroup) probe(streams []string, now time.Time, spacing time.Duration) (string, probeDecision) {
	if !g.paused() {
		return "", probeNotPaused
	}

	g.spacing = spacing

	if now.Before(g.probeAfter) || len(streams) == 0 {
		return "", probeWaiting
	}

	if g.rejectedProbes >= g.fallbackThreshold() {
		g.probeAfter = time.Time{}

		return "", probeGivenUp
	}

	stream := streams[g.probes%len(streams)]
	g.probes++

	return stream, probeDue
}

// blame pauses requests until the first probe is due and reports whether that is news. Blame the
// session already held is not: reaching here twice means a fallback bisected the fleet and found
// nothing that answers. Only the probes of the new pause count towards abandoning it.
func (g *logGroup) blame(now time.Time, spacing time.Duration) bool {
	g.spacing = spacing
	g.probeAfter = now.Add(spacing)
	g.rejectedProbes = 0

	if g.blamed {
		g.unproductiveFallbacks = min(g.unproductiveFallbacks+1, maxProbeBackoff)

		return false
	}

	g.blamed = true

	return true
}

// noteProbeRejected extends the pause to the next probe and counts the rejection against the group,
// since a rejection over existence is the one answer that may be the group's doing. It is the only
// failure that moves the pause: a probe that was throttled or cut short by the deadline was not
// heard from, and holding every instance in the session back for it would let rate limiting alone
// stretch an outage. The rotation has moved on, so the next scrape asks the next stream instead, at
// the one request a probe costs.
func (g *logGroup) noteProbeRejected(now time.Time) {
	g.probeAfter = now.Add(g.spacing)
	g.rejectedProbes++
}

// noteAnswered records that the group exists and is not the suspect any more, so that the next time
// it is blamed is a new outage whose first fallback is worth paying for again. It reports whether
// that cleared the group of blame, which covers ending a pause: a fallback gives the pause up and
// keeps the blame, and the answer that clears it is news either way.
func (g *logGroup) noteAnswered() bool {
	recovered := g.blamed

	g.seen = true
	g.blamed = false
	g.unproductiveFallbacks = 0
	g.probeAfter = time.Time{}

	return recovered
}
