package enhanced

import (
	"time"
)

const (
	missingStreamTTL   = 5 * time.Minute
	maxProbesPerScrape = 8
	halves             = 2

	// maxIsolationCalls bounds the extra requests one batch may spend on finding its missing streams,
	// so a region where every stream is missing converges over a few scrapes instead of flooding AWS.
	maxIsolationCalls = 32
)

// missingStreams tracks the log streams CloudWatch reported as non-existent. It is only used from
// the scrape goroutine, so it needs no lock.
type missingStreams struct {
	probeAfter map[string]time.Time // log stream name -> earliest time to try it again
}

func newMissingStreams() *missingStreams {
	return &missingStreams{probeAfter: make(map[string]time.Time)}
}

// mark excludes a log stream from later requests and reports whether it was not excluded already.
func (m *missingStreams) mark(name string, now time.Time) bool {
	_, known := m.probeAfter[name]
	m.probeAfter[name] = now.Add(missingStreamTTL)

	return !known
}

// clear stops excluding a log stream and reports whether it was excluded.
func (m *missingStreams) clear(name string) bool {
	_, known := m.probeAfter[name]
	delete(m.probeAfter, name)

	return known
}

// marked reports whether a log stream is currently excluded.
func (m *missingStreams) marked(name string) bool {
	_, known := m.probeAfter[name]

	return known
}

// due reports whether an excluded log stream should be tried again.
func (m *missingStreams) due(name string, now time.Time) bool {
	probeAfter, known := m.probeAfter[name]

	return !known || now.After(probeAfter)
}

func (m *missingStreams) len() int {
	return len(m.probeAfter)
}
