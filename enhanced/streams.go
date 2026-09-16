package enhanced

import (
	"time"
)

const (
	missingStreamTTL   = 5 * time.Minute
	maxProbesPerScrape = 8
	bisectDivisor      = 2

	// maxIsolationCalls bounds the requests one batch may spend on finding its missing streams. It is
	// the exact cost of the worst case rather than a figure picked to be frugal: a bisect issues
	// bisectDivisor requests per split and needs n-1 splits to single out n streams, so a batch of
	// maxLogStreamsPerRequest in which every stream is missing costs this and can cost no more.
	// Budgeting less is what makes a batch unattributable: the bisect stops half way, the streams it
	// did reach are excluded for a fault they may not have, and a missing log group -- which rejects
	// every request and so is only recognisable once every stream has been singled out -- can never
	// be recognised for a full batch at all. A half holding no missing stream is answered rather than
	// halved, so the ordinary case of a few missing streams costs a fraction of this.
	maxIsolationCalls = bisectDivisor * (maxLogStreamsPerRequest - 1)
)

// missingStreams tracks the log streams CloudWatch reported as non-existent. It is only used from
// the scrape goroutine, so it needs no lock.
type missingStreams struct {
	probeAfter map[string]time.Time // log stream name -> earliest time to try it again
	// tentative holds the streams excluded by a scrape no request of which was answered. A rejection
	// names no stream, and a scrape answered nothing anywhere cannot tell a stream that is gone from
	// a log group that is: only a request the group answers settles that, so these wait for one.
	tentative map[string]struct{}
}

func newMissingStreams() *missingStreams {
	return &missingStreams{
		probeAfter: make(map[string]time.Time),
		tentative:  make(map[string]struct{}),
	}
}

// mark excludes a log stream from later requests and reports whether it was not excluded already.
// The exclusion is tentative when the scrape that made it was not answered anywhere, and the latest
// rejection decides: what a stream was excluded on before says nothing about what rejected it now.
func (m *missingStreams) mark(name string, now time.Time, tentative bool) bool {
	_, known := m.probeAfter[name]
	m.probeAfter[name] = now.Add(missingStreamTTL)

	if tentative {
		m.tentative[name] = struct{}{}
	} else {
		delete(m.tentative, name)
	}

	return !known
}

// clear stops excluding a log stream and reports whether it was excluded.
func (m *missingStreams) clear(name string) bool {
	_, known := m.probeAfter[name]
	delete(m.probeAfter, name)
	delete(m.tentative, name)

	return known
}

// releaseTentative stops excluding every stream whose exclusion was tentative and reports how many
// there were. Called once the log group has answered, which is the evidence those exclusions were
// waiting for: a stream among them that is still rejected is then rejected for a reason of its own.
func (m *missingStreams) releaseTentative() int {
	released := len(m.tentative)

	for name := range m.tentative {
		delete(m.probeAfter, name)
	}

	clear(m.tentative)

	return released
}

func (m *missingStreams) marked(name string) bool {
	_, known := m.probeAfter[name]

	return known
}

// due reports whether an excluded log stream may be probed again. A stream that is not excluded is
// never due, because nothing is holding it back.
func (m *missingStreams) due(name string, now time.Time) bool {
	probeAfter, known := m.probeAfter[name]

	return known && now.After(probeAfter)
}

func (m *missingStreams) len() int {
	return len(m.probeAfter)
}
