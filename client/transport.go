package client

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
)

type transport struct {
	t *http.Transport
	l log.Logger

	mRequests  prometheus.Counter
	mResponses *prometheus.SummaryVec
}

func newTransport() *transport {
	return &transport{
		t: &http.Transport{
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     2 * time.Minute,
			Proxy:               http.ProxyFromEnvironment,
		},
		l: log.With("component", "transport"),

		mRequests: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "rds_exporter_requests_total",
			Help: "Total number of AWS API requests.",
		}),
		mResponses: prometheus.NewSummaryVec(prometheus.SummaryOpts{
			Name: "rds_exporter_responses_durations_seconds",
			Help: "AWS API responses latency distributions.",
		}, []string{"status"}),
	}
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// We could use "net/http/httptrace" package if we ever need more metrics.

	start := time.Now()
	t.mRequests.Inc()
	resp, err := t.t.RoundTrip(req)
	duration := time.Since(start)
	if resp != nil {
		t.mResponses.WithLabelValues(strconv.Itoa(resp.StatusCode)).Observe(duration.Seconds())
		t.l.Debugf("%s %s -> %d (%s)", req.Method, req.URL.String(), resp.StatusCode, duration)
	} else {
		t.mResponses.WithLabelValues("err").Observe(duration.Seconds())
		t.l.Errorf("%s %s -> %s (%s)", req.Method, req.URL.String(), err, duration)
	}
	return resp, err
}

// check interface
var _ http.RoundTripper = (*transport)(nil)
