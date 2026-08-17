package enhanced

import (
	"sync"
	"testing"
	"time"

	"github.com/percona/exporter_shared/helpers"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/promlog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/percona/rds_exporter/sessions"
)

const osMetricName = "node_cpu_average"

func testKey(instance string) instanceKey {
	return instanceKey{region: testRegion, instance: instance}
}

func testCollector(states map[instanceKey]instanceState) *Collector {
	collector := newCollector(promlog.New(&promlog.Config{}))
	collector.metrics = states

	return collector
}

// configuredCollector returns a collector monitoring the named instances, whether or not they have
// ever delivered a sample.
func configuredCollector(states map[instanceKey]instanceState, instances ...string) *Collector {
	collector := testCollector(states)
	for _, instance := range instances {
		collector.configured[testKey(instance)] = struct{}{}
	}

	return collector
}

// sampleMetrics returns one OS metric carrying the labels the collector must not alter.
func sampleMetrics(instance string) []prometheus.Metric {
	desc := prometheus.NewDesc(osMetricName, "The percentage of CPU in use.", nil, prometheus.Labels{
		regionLabel:   testRegion,
		instanceLabel: instance,
	})

	return []prometheus.Metric{prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 1)}
}

func collect(t *testing.T, collector *Collector) []*helpers.Metric {
	t.Helper()

	ch := make(chan prometheus.Metric, 100)
	collector.Collect(ch)
	close(ch)

	collected := make([]prometheus.Metric, 0, len(ch))
	for metric := range ch {
		collected = append(collected, metric)
	}

	return helpers.ReadMetrics(collected)
}

func findMetric(metrics []*helpers.Metric, name, instance string) *helpers.Metric {
	for _, metric := range metrics {
		if metric.Name == name && metric.Labels[instanceLabel] == instance {
			return metric
		}
	}

	return nil
}

func TestConfigure(t *testing.T) {
	t.Parallel()

	t.Run("covers every session before the first scraper starts", func(t *testing.T) {
		t.Parallel()

		disabled := testInstance("disabled", "disabled-resource-id")
		disabled.DisableEnhancedMetrics = true

		collector := newCollector(promlog.New(&promlog.Config{}))
		enabled := collector.configure(map[string][]sessions.Instance{
			"session-a": {testInstance("primary", oldResourceID)},
			"session-b": {testInstance("replica", newResourceID), disabled},
		})

		// prune reads the set from the drain goroutine of a session that is already scraping, so no
		// session may still be missing from it by then.
		assert.Equal(t, map[instanceKey]struct{}{
			testKey("primary"): {},
			testKey("replica"): {},
		}, collector.configured)
		assert.Len(t, enabled["session-b"], 1, "an instance PMM disabled must not be scraped")
	})
}

func TestCollect(t *testing.T) { //nolint:funlen
	t.Parallel()

	t.Run("skips expired metrics", func(t *testing.T) {
		t.Parallel()

		eventTime := time.Now().Add(-time.Minute)
		collector := testCollector(map[instanceKey]instanceState{
			testKey("fresh"): {
				metrics:    sampleMetrics("fresh"),
				eventTime:  eventTime,
				expiresAt:  time.Now().Add(time.Minute),
				receivedAt: time.Now(),
			},
			testKey("expired"): {
				metrics:    sampleMetrics("expired"),
				eventTime:  eventTime,
				expiresAt:  time.Now().Add(-time.Minute),
				receivedAt: time.Now(),
			},
		})

		metrics := collect(t, collector)

		assert.NotNil(t, findMetric(metrics, osMetricName, "fresh"))
		assert.Nil(t, findMetric(metrics, osMetricName, "expired"),
			"stale values must render as a gap instead of a flat line")

		fresh := findMetric(metrics, upMetricName, "fresh")
		require.NotNil(t, fresh)
		assert.InDelta(t, 1.0, fresh.Value, 0)

		expired := findMetric(metrics, upMetricName, "expired")
		require.NotNil(t, expired)
		assert.InDelta(t, 0.0, expired.Value, 0, "an outage must stay alertable on a value, not on absence")
	})

	t.Run("reports instances that never delivered", func(t *testing.T) {
		t.Parallel()

		collector := configuredCollector(map[instanceKey]instanceState{
			testKey("reporting"): {
				metrics:    sampleMetrics("reporting"),
				eventTime:  time.Now().Add(-time.Minute),
				expiresAt:  time.Now().Add(time.Minute),
				receivedAt: time.Now(),
			},
		}, "reporting", "unmonitored")

		metrics := collect(t, collector)

		unmonitored := findMetric(metrics, upMetricName, "unmonitored")
		require.NotNil(t, unmonitored, "an instance without Enhanced Monitoring must still be alertable")
		assert.InDelta(t, 0.0, unmonitored.Value, 0)
		assert.Nil(t, findMetric(metrics, osMetricName, "unmonitored"))

		reporting := findMetric(metrics, upMetricName, "reporting")
		require.NotNil(t, reporting)
		assert.InDelta(t, 1.0, reporting.Value, 0)
	})

	t.Run("emits the self metrics", func(t *testing.T) {
		t.Parallel()

		eventTime := time.Now().Add(-time.Minute).Truncate(time.Second)
		collector := testCollector(map[instanceKey]instanceState{
			testKey("primary"): {
				metrics:    sampleMetrics("primary"),
				eventTime:  eventTime,
				expiresAt:  time.Now().Add(time.Minute),
				receivedAt: time.Now(),
			},
		})
		collector.errors[errorKey{region: testRegion, kind: errorKindThrottling}] = 3

		metrics := collect(t, collector)

		up := findMetric(metrics, upMetricName, "primary")
		require.NotNil(t, up)
		assert.InDelta(t, 1.0, up.Value, 0)
		assert.Equal(t, prometheus.Labels{regionLabel: testRegion, instanceLabel: "primary"}, up.Labels)

		lastEvent := findMetric(metrics, lastEventMetricName, "primary")
		require.NotNil(t, lastEvent)
		assert.InDelta(t, float64(eventTime.Unix()), lastEvent.Value, 0)
		assert.Equal(t, prometheus.Labels{regionLabel: testRegion, instanceLabel: "primary"}, lastEvent.Labels)

		var errorsMetric *helpers.Metric

		for _, metric := range metrics {
			if metric.Name == scrapeErrorsMetricName {
				errorsMetric = metric
			}
		}

		require.NotNil(t, errorsMetric)
		assert.InDelta(t, 3.0, errorsMetric.Value, 0)
		assert.Equal(t, prometheus.Labels{regionLabel: testRegion, kindLabel: errorKindThrottling}, errorsMetric.Labels)
	})

	t.Run("runs concurrently with the scrapers", func(t *testing.T) {
		t.Parallel()

		collector := testCollector(map[instanceKey]instanceState{})

		var waitGroup sync.WaitGroup

		for iteration := range 20 {
			waitGroup.Add(2)

			go func() {
				defer waitGroup.Done()

				collector.setMetrics(scrapeResult{
					metrics: map[instanceKey]instanceMetrics{
						testKey("primary"): {
							metrics:   sampleMetrics("primary"),
							eventTime: time.Now().Add(time.Duration(iteration) * time.Second),
						},
					},
					errorCounts: map[string]uint64{errorKindOther: 1},
					region:      testRegion,
					interval:    time.Minute,
				}, time.Now())
			}()

			go func() {
				defer waitGroup.Done()

				collect(t, collector)
			}()
		}

		waitGroup.Wait()
	})
}

func TestSetMetrics(t *testing.T) { //nolint:funlen
	t.Parallel()

	t.Run("follows the interval AWS reports", func(t *testing.T) {
		t.Parallel()

		eventTime := time.Now().Add(-time.Minute)
		collector := testCollector(map[instanceKey]instanceState{})

		collector.setMetrics(scrapeResult{
			metrics: map[instanceKey]instanceMetrics{
				testKey("primary"): {metrics: sampleMetrics("primary"), eventTime: eventTime},
			},
			errorCounts: nil,
			region:      testRegion,
			interval:    10 * time.Minute,
		}, time.Now())

		assert.Equal(t, eventTime.Add(metricsTTL(10*time.Minute)), collector.metrics[testKey("primary")].expiresAt,
			"expiry must follow the interval AWS reports now, not the one reported at startup")
	})

	t.Run("ignores a redelivered event", func(t *testing.T) {
		t.Parallel()

		eventTime := time.Now().Add(-time.Minute)
		collector := testCollector(map[instanceKey]instanceState{})
		result := scrapeResult{
			metrics: map[instanceKey]instanceMetrics{
				testKey("primary"): {metrics: sampleMetrics("primary"), eventTime: eventTime},
			},
			errorCounts: nil,
			region:      testRegion,
			interval:    time.Minute,
		}

		collector.setMetrics(result, time.Now())
		firstExpiry := collector.metrics[testKey("primary")].expiresAt

		// FilterLogEvents StartTime is inclusive, so the newest event of the slowest instance comes back
		// on every scrape. Expiry must follow the event timestamp, not the wall clock.
		collector.setMetrics(result, time.Now())

		assert.Equal(t, firstExpiry, collector.metrics[testKey("primary")].expiresAt)
	})

	t.Run("restores an instance whose payload was released", func(t *testing.T) {
		t.Parallel()

		lastEvent := time.Now().Add(-staleRetention - time.Minute)
		collector := configuredCollector(map[instanceKey]instanceState{
			testKey("promoted"): {
				metrics:    nil,
				eventTime:  lastEvent,
				expiresAt:  lastEvent.Add(minMetricsTTL),
				receivedAt: lastEvent,
			},
		}, "promoted")

		// A promoted instance whose clock trails the retired one's by more than the retention publishes
		// events older than the last one stored, which the timestamp guard alone would refuse for good.
		older := lastEvent.Add(-time.Hour)
		collector.setMetrics(scrapeResult{
			metrics: map[instanceKey]instanceMetrics{
				testKey("promoted"): {metrics: sampleMetrics("promoted"), eventTime: older},
			},
			errorCounts: nil,
			region:      testRegion,
			interval:    time.Minute,
		}, time.Now())

		state := collector.metrics[testKey("promoted")]
		assert.NotNil(t, state.metrics, "an instance with no payload left must be able to start reporting again")
		assert.Equal(t, older, state.eventTime)
	})

	t.Run("removes long expired instances", func(t *testing.T) {
		t.Parallel()

		collector := testCollector(map[instanceKey]instanceState{
			testKey("retired"): {
				metrics:    sampleMetrics("retired"),
				eventTime:  time.Now().Add(-staleRetention - time.Minute),
				expiresAt:  time.Now().Add(-staleRetention),
				receivedAt: time.Now().Add(-staleRetention - time.Minute),
			},
		})

		collector.setMetrics(scrapeResult{metrics: nil, errorCounts: nil, region: testRegion, interval: time.Minute}, time.Now())

		assert.Empty(t, collector.metrics, "an instance no longer configured must eventually disappear")
	})

	t.Run("replaces an instance after a resource ID change", func(t *testing.T) {
		t.Parallel()

		collector := testCollector(map[instanceKey]instanceState{})
		key := testKey("promoted")

		collector.setMetrics(scrapeResult{
			metrics: map[instanceKey]instanceMetrics{
				key: {metrics: sampleMetrics("promoted"), eventTime: time.Now().Add(-time.Minute)},
			},
			errorCounts: nil,
			region:      testRegion,
			interval:    time.Minute,
		}, time.Now())
		collector.setMetrics(scrapeResult{
			metrics:     map[instanceKey]instanceMetrics{key: {metrics: sampleMetrics("promoted"), eventTime: time.Now()}},
			errorCounts: nil,
			region:      testRegion,
			interval:    time.Minute,
		}, time.Now())

		require.Len(t, collector.metrics, 1, "a switchover must replace the instance, not duplicate its label set")

		metrics := collect(t, collector)

		ups := 0

		for _, metric := range metrics {
			if metric.Name == upMetricName {
				ups++
			}
		}

		assert.Equal(t, 1, ups)
	})
}

func TestPrune(t *testing.T) {
	t.Parallel()

	t.Run("keeps configured instances reported", func(t *testing.T) {
		t.Parallel()

		eventTime := time.Now().Add(-staleRetention - time.Minute)
		collector := configuredCollector(map[instanceKey]instanceState{
			testKey("down"): {
				metrics:    sampleMetrics("down"),
				eventTime:  eventTime,
				expiresAt:  time.Now().Add(-staleRetention),
				receivedAt: eventTime,
			},
		}, "down")

		collector.setMetrics(scrapeResult{metrics: nil, errorCounts: nil, region: testRegion, interval: time.Minute}, time.Now())

		metrics := collect(t, collector)

		down := findMetric(metrics, upMetricName, "down")
		require.NotNil(t, down, "an outage longer than the retention must not resolve the alert by itself")
		assert.InDelta(t, 0.0, down.Value, 0)
		assert.Nil(t, findMetric(metrics, osMetricName, "down"), "the stale payload must be released")

		lastEvent := findMetric(metrics, lastEventMetricName, "down")
		require.NotNil(t, lastEvent, "support needs to know when the instance was last seen")
		assert.InDelta(t, float64(eventTime.Unix()), lastEvent.Value, 0)
	})

	t.Run("keeps a sample until the retention has passed", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		collector := configuredCollector(map[instanceKey]instanceState{
			testKey("borderline"): {
				metrics:    sampleMetrics("borderline"),
				eventTime:  now.Add(-staleRetention),
				expiresAt:  now,
				receivedAt: now.Add(-staleRetention),
			},
		}, "borderline")

		collector.prune(now)

		assert.NotNil(t, collector.metrics[testKey("borderline")].metrics, "the retention is inclusive")

		collector.prune(now.Add(time.Nanosecond))

		assert.Nil(t, collector.metrics[testKey("borderline")].metrics)
	})
}

func TestMetricsTTL(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		interval    time.Duration
		expectedTTL time.Duration
	}{
		{interval: 2 * time.Second, expectedTTL: minMetricsTTL},
		{interval: 10 * time.Second, expectedTTL: minMetricsTTL},
		{interval: time.Minute, expectedTTL: minMetricsTTL},
		{interval: 5 * time.Minute, expectedTTL: 15 * time.Minute},
	} {
		t.Run(testCase.interval.String(), func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, testCase.expectedTTL, metricsTTL(testCase.interval))
		})
	}
}
