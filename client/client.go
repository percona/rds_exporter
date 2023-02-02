package client

import (
	"net/http"
	"time"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

// Client represents HTTP client for all AWS APIs with metrics reporting.
type Client struct {
	c *http.Client
	t *transport
}

// New creates new Client.
func New(logger log.Logger) *Client {
	t := newTransport(logger)
	return &Client{
		c: &http.Client{
			Transport: t,
			Timeout:   15 * time.Second,
		},
		t: t,
	}
}

// HTTP returns underlying *http.Client.
func (c *Client) HTTP() *http.Client {
	return c.c
}

// Describe implements prometheus.Collector.
func (c *Client) Describe(ch chan<- *prometheus.Desc) {
	c.t.mRequests.Describe(ch)
}

// Collect implements prometheus.Collector.
func (c *Client) Collect(ch chan<- prometheus.Metric) {
	c.t.mRequests.Collect(ch)
}

// check interfaces
var (
	_ prometheus.Collector = (*Client)(nil)
)
