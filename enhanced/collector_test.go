package enhanced

import (
	"context"
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

const (
	osMetricName = "node_cpu_average"

	sessionA = "session-a"
	sessionB = "session-b"

	// Two accounts monitored in one region, each with an instance of the same name.
	firstSession  = "us-east-1/first"
	secondSession = "us-east-1/second"
	firstAccount  = "first"
	secondAccount = "second"
	accountLabel  = "account"
	sharedName    = "prod-db"
)

func testKey(instance string) instanceKey {
	return instanceKey{session: testSession, region: testRegion, instance: instance}
}

func testCollector(states map[instanceKey]storedSample) *Collector {
	collector := newCollector(promlog.New(&promlog.Config{}))
	collector.metrics = states

	return collector
}

// configuredCollector returns a collector monitoring the named instances, whether or not they have
// ever delivered a sample.
func configuredCollector(states map[instanceKey]storedSample, instances ...string) *Collector {
	collector := testCollector(states)
	for _, instance := range instances {
		collector.configured[testKey(instance)] = instanceLabels(testRegion, instance, nil)
	}

	return collector
}

// monitoredCollector returns a collector whose last scrape reported the given Enhanced Monitoring
// state per instance, as AWS has it rather than as the config asks for it.
func monitoredCollector(states map[instanceKey]storedSample, monitored map[string]bool) *Collector {
	instances := make([]string, 0, len(monitored))
	for instance := range monitored {
		instances = append(instances, instance)
	}

	collector := configuredCollector(states, instances...)
	for instance, on := range monitored {
		collector.monitored[testKey(instance)] = on
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

// collectSamplesAt collects the stored samples as of a chosen moment, so a test can reach an expiry
// without waiting for it.
func collectSamplesAt(t *testing.T, collector *Collector, now time.Time) []*helpers.Metric {
	t.Helper()

	ch := make(chan prometheus.Metric, 100)
	collector.collectSamples(ch, now)
	close(ch)

	collected := make([]prometheus.Metric, 0, len(ch))
	for metric := range ch {
		collected = append(collected, metric)
	}

	return helpers.ReadMetrics(collected)
}

// findMetricByName returns the metric of the given name, for the ones carrying no instance label.
func findMetricByName(metrics []*helpers.Metric, name string) *helpers.Metric {
	for _, metric := range metrics {
		if metric.Name == name {
			return metric
		}
	}

	return nil
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

		primary := testInstance("primary", oldResourceID)
		replica := testInstance("replica", newResourceID)

		collector := newCollector(promlog.New(&promlog.Config{}))
		enabled := collector.configure(map[string][]sessions.Instance{
			sessionA: {primary},
			sessionB: {replica, disabled},
		})

		// prune reads the set from the drain goroutine of a session that is already scraping, so no
		// session may still be missing from it by then.
		assert.Equal(t, map[instanceKey]prometheus.Labels{
			keyOf(sessionA, primary): instanceLabels(testRegion, primary.Instance, nil),
			keyOf(sessionB, replica): instanceLabels(testRegion, replica.Instance, nil),
		}, collector.configured)
		assert.Len(t, enabled[sessionB], 1, "an instance PMM disabled must not be scraped")
	})

	t.Run("leaves out a session with nothing to scrape", func(t *testing.T) {
		t.Parallel()

		disabled := testInstance("disabled", "disabled-resource-id")
		disabled.DisableEnhancedMetrics = true

		collector := newCollector(promlog.New(&promlog.Config{}))
		enabled := collector.configure(map[string][]sessions.Instance{
			sessionA: {disabled},
		})

		// A scraper for such a session has no log stream to request and no region to report under, so
		// it would publish its self-metrics with an empty region label and poll AWS for nothing.
		assert.NotContains(t, enabled, sessionA)
		assert.Empty(t, collector.configured)
	})
}

func TestConfigureKeepsTheSameInstanceOfTwoAccountsApart(t *testing.T) {
	t.Parallel()

	// A DB identifier is only unique within an account, so two sessions monitoring one region can
	// each have an instance of the same name. Their samples must not overwrite each other, and their
	// health must be told apart the way their metrics are: by the configured labels.
	first := testInstance(sharedName, oldResourceID)
	first.Labels = map[string]string{accountLabel: firstAccount}
	second := testInstance(sharedName, newResourceID)
	second.Labels = map[string]string{accountLabel: secondAccount}

	collector := newCollector(promlog.New(&promlog.Config{}))
	collector.configure(map[string][]sessions.Instance{
		firstSession:  {first},
		secondSession: {second},
	})

	assert.Equal(t, map[instanceKey]prometheus.Labels{
		{session: firstSession, region: testRegion, instance: sharedName}: {
			regionLabel: testRegion, instanceLabel: sharedName, accountLabel: firstAccount,
		},
		{session: secondSession, region: testRegion, instance: sharedName}: {
			regionLabel: testRegion, instanceLabel: sharedName, accountLabel: secondAccount,
		},
	}, collector.configured)
}

func TestCollect(t *testing.T) {
	t.Parallel()

	t.Run("skips expired metrics", func(t *testing.T) {
		t.Parallel()

		eventTime := time.Now().Add(-time.Minute)
		collector := testCollector(map[instanceKey]storedSample{
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

		collector := configuredCollector(map[instanceKey]storedSample{
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
		collector := testCollector(map[instanceKey]storedSample{
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

	t.Run("labels health like the samples", func(t *testing.T) {
		t.Parallel()

		// Two accounts' instances of the same name only differ by the labels the configuration gives
		// them, and health reported by region and name alone would be one series for the two.
		firstKey := instanceKey{session: firstSession, region: testRegion, instance: sharedName}
		secondKey := instanceKey{session: secondSession, region: testRegion, instance: sharedName}
		collector := testCollector(map[instanceKey]storedSample{
			firstKey: {
				metrics:    sampleMetrics(sharedName),
				eventTime:  time.Now().Add(-time.Minute),
				expiresAt:  time.Now().Add(time.Minute),
				receivedAt: time.Now(),
			},
		})
		collector.configured[firstKey] = instanceLabels(testRegion, sharedName, map[string]string{accountLabel: firstAccount})
		collector.configured[secondKey] = instanceLabels(testRegion, sharedName, map[string]string{accountLabel: secondAccount})

		metrics := collect(t, collector)

		ups := make(map[string]float64)

		for _, metric := range metrics {
			if metric.Name == upMetricName {
				ups[metric.Labels[accountLabel]] = metric.Value
			}
		}

		assert.Equal(t, map[string]float64{firstAccount: 1, secondAccount: 0}, ups,
			"each account's instance must report its own health")

		lastEvent := findMetric(metrics, lastEventMetricName, sharedName)
		require.NotNil(t, lastEvent)
		assert.Equal(t, firstAccount, lastEvent.Labels[accountLabel])
	})

	t.Run("counts a wrong clock outside the error metric", func(t *testing.T) {
		t.Parallel()

		collector := testCollector(map[instanceKey]storedSample{})

		collector.setMetrics(scrapeResult{
			metrics:      nil,
			errorCounts:  nil,
			skewedEvents: 4,
			monitored:    nil,
			region:       testRegion,
			interval:     time.Minute,
		}, time.Now())

		metrics := collect(t, collector)

		// Nothing failed: the samples were exported, so a wrong clock may not read as failing
		// collection to anything alerting on the error counter.
		skew := findMetricByName(metrics, clockSkewMetricName)
		require.NotNil(t, skew)
		assert.InDelta(t, 4.0, skew.Value, 0)
		assert.Equal(t, prometheus.Labels{regionLabel: testRegion}, skew.Labels)
		assert.Nil(t, findMetricByName(metrics, scrapeErrorsMetricName))
	})

	t.Run("runs concurrently with the scrapers", func(t *testing.T) {
		t.Parallel()

		collector := testCollector(map[instanceKey]storedSample{})

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
					errorCounts:  map[string]uint64{errorKindOther: 1},
					skewedEvents: 0,
					monitored:    nil,
					region:       testRegion,
					interval:     time.Minute,
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

func TestCollectSilentInstances(t *testing.T) {
	t.Parallel()

	t.Run("reports a configured instance that never delivered a sample", func(t *testing.T) {
		t.Parallel()

		collector := monitoredCollector(map[instanceKey]storedSample{}, map[string]bool{"silent": true})

		up := findMetric(collect(t, collector), upMetricName, "silent")

		require.NotNil(t, up, "a missing log stream must be alertable without absent()")
		assert.InDelta(t, 0.0, up.Value, 0)
	})

	t.Run("says nothing about an instance AWS has Enhanced Monitoring off for", func(t *testing.T) {
		t.Parallel()

		collector := monitoredCollector(map[instanceKey]storedSample{}, map[string]bool{"unmonitored": false})

		// enhancedStreams requests no stream for it, so a sample was never due. Asserting down would be
		// a standing false alarm that no change to the exporter's own config could clear.
		assert.Nil(t, findMetric(collect(t, collector), upMetricName, "unmonitored"))
	})

	t.Run("stops reporting an instance whose Enhanced Monitoring is turned off", func(t *testing.T) {
		t.Parallel()

		lastEvent := time.Now().Add(-2 * minMetricsTTL)
		collector := monitoredCollector(map[instanceKey]storedSample{
			testKey("retired"): {
				metrics:    nil,
				eventTime:  lastEvent,
				expiresAt:  lastEvent.Add(minMetricsTTL),
				receivedAt: lastEvent,
			},
		}, map[string]bool{"retired": false})

		metrics := collect(t, collector)

		// The same unclearable zero reached through the stale entry rather than the silent set.
		assert.Nil(t, findMetric(metrics, upMetricName, "retired"))
		assert.NotNil(t, findMetric(metrics, lastEventMetricName, "retired"),
			"when the instance last reported is still worth knowing")
	})
}

// futureResult returns a scrape carrying one sample for "primary", dated whenever the caller says.
func futureResult(eventTime time.Time) scrapeResult {
	return scrapeResult{
		metrics: map[instanceKey]instanceMetrics{
			testKey("primary"): {metrics: sampleMetrics("primary"), eventTime: eventTime},
		},
		errorCounts:  nil,
		skewedEvents: 0,
		monitored:    map[instanceKey]bool{testKey("primary"): true},
		region:       testRegion,
		interval:     time.Minute,
	}
}

// TestSetMetrics covers what a stored sample does to the instance it belongs to.
func TestSetMetrics(t *testing.T) {
	t.Parallel()

	t.Run("follows the interval AWS reports", func(t *testing.T) {
		t.Parallel()

		eventTime := time.Now().Add(-time.Minute)
		collector := testCollector(map[instanceKey]storedSample{})

		collector.setMetrics(scrapeResult{
			metrics: map[instanceKey]instanceMetrics{
				testKey("primary"): {metrics: sampleMetrics("primary"), eventTime: eventTime},
			},
			errorCounts:  nil,
			skewedEvents: 0,
			monitored:    nil,
			region:       testRegion,
			interval:     10 * time.Minute,
		}, time.Now())

		assert.Equal(t, eventTime.Add(metricsTTL(10*time.Minute)), collector.metrics[testKey("primary")].expiresAt,
			"expiry must follow the interval AWS reports now, not the one reported at startup")
	})

	t.Run("restores an instance whose payload was released", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		received := now.Add(-staleRetention - time.Minute)
		collector := configuredCollector(map[instanceKey]storedSample{
			testKey("promoted"): {
				metrics:    nil,
				eventTime:  received.Add(time.Hour),
				expiresAt:  received.Add(minMetricsTTL),
				receivedAt: received,
			},
		}, "promoted")

		// The retired instance dated its last event an hour ahead, so every event the promoted one
		// publishes is older than what is stored. Prune keeps that timestamp after releasing the
		// payload, and refusing the promoted instance's events on it would strand the instance for good.
		eventTime := now.Add(-time.Minute)
		collector.setMetrics(scrapeResult{
			metrics: map[instanceKey]instanceMetrics{
				testKey("promoted"): {metrics: sampleMetrics("promoted"), eventTime: eventTime},
			},
			errorCounts:  nil,
			skewedEvents: 0,
			monitored:    nil,
			region:       testRegion,
			interval:     time.Minute,
		}, now)

		state := collector.metrics[testKey("promoted")]
		assert.NotNil(t, state.metrics, "an instance with no payload left must be able to start reporting again")
		assert.Equal(t, eventTime, state.eventTime)
	})

	t.Run("removes long expired instances", func(t *testing.T) {
		t.Parallel()

		collector := testCollector(map[instanceKey]storedSample{
			testKey("retired"): {
				metrics:    sampleMetrics("retired"),
				eventTime:  time.Now().Add(-staleRetention - time.Minute),
				expiresAt:  time.Now().Add(-staleRetention),
				receivedAt: time.Now().Add(-staleRetention - time.Minute),
			},
		})

		collector.setMetrics(scrapeResult{
			metrics:      nil,
			errorCounts:  nil,
			skewedEvents: 0,
			monitored:    nil,
			region:       testRegion,
			interval:     time.Minute,
		}, time.Now())

		assert.Empty(t, collector.metrics, "an instance no longer configured must eventually disappear")
	})

	t.Run("replaces an instance after a resource ID change", func(t *testing.T) {
		t.Parallel()

		collector := testCollector(map[instanceKey]storedSample{})
		key := testKey("promoted")

		collector.setMetrics(scrapeResult{
			metrics: map[instanceKey]instanceMetrics{
				key: {metrics: sampleMetrics("promoted"), eventTime: time.Now().Add(-time.Minute)},
			},
			errorCounts:  nil,
			skewedEvents: 0,
			monitored:    nil,
			region:       testRegion,
			interval:     time.Minute,
		}, time.Now())
		collector.setMetrics(scrapeResult{
			metrics:      map[instanceKey]instanceMetrics{key: {metrics: sampleMetrics("promoted"), eventTime: time.Now()}},
			errorCounts:  nil,
			skewedEvents: 0,
			monitored:    nil,
			region:       testRegion,
			interval:     time.Minute,
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

	t.Run("follows Enhanced Monitoring being turned off in AWS", func(t *testing.T) {
		t.Parallel()

		key := testKey("primary")
		collector := configuredCollector(map[instanceKey]storedSample{}, "primary")

		collector.setMetrics(scrapeResult{
			metrics:      nil,
			errorCounts:  nil,
			skewedEvents: 0,
			monitored:    map[instanceKey]bool{key: true},
			region:       testRegion,
			interval:     time.Minute,
		}, time.Now())
		require.False(t, collector.silenced(key))

		collector.setMetrics(scrapeResult{
			metrics:      nil,
			errorCounts:  nil,
			skewedEvents: 0,
			monitored:    map[instanceKey]bool{key: false},
			region:       testRegion,
			interval:     time.Minute,
		}, time.Now())

		assert.True(t, collector.silenced(key), "the scrapers report the state AWS has now, not at startup")
	})
}

// TestSetMetricsEventTime covers which event timestamp a stored sample is judged by, on a clock
// the monitored account controls.
func TestSetMetricsEventTime(t *testing.T) {
	t.Parallel()

	t.Run("ignores a redelivered event", func(t *testing.T) {
		t.Parallel()

		eventTime := time.Now().Add(-time.Minute)
		collector := testCollector(map[instanceKey]storedSample{})
		result := scrapeResult{
			metrics: map[instanceKey]instanceMetrics{
				testKey("primary"): {metrics: sampleMetrics("primary"), eventTime: eventTime},
			},
			errorCounts:  nil,
			skewedEvents: 0,
			monitored:    nil,
			region:       testRegion,
			interval:     time.Minute,
		}

		collector.setMetrics(result, time.Now())
		firstExpiry := collector.metrics[testKey("primary")].expiresAt

		// FilterLogEvents StartTime is inclusive, so the newest event of the slowest instance comes back
		// on every scrape. Expiry must follow the event timestamp, not the wall clock.
		collector.setMetrics(result, time.Now())

		assert.Equal(t, firstExpiry, collector.metrics[testKey("primary")].expiresAt)
	})

	t.Run("accepts a real event stored after a future dated one", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		collector := configuredCollector(map[instanceKey]storedSample{}, "primary")

		// CloudWatch accepts event timestamps up to two hours ahead, and an exporter whose clock is
		// behind AWS has nothing else to judge the instance by, so one gets stored.
		collector.setMetrics(futureResult(now.Add(90*time.Minute)), now)

		eventTime := now.Add(2 * time.Minute)
		collector.setMetrics(futureResult(eventTime), eventTime)

		state := collector.metrics[testKey("primary")]
		require.Equal(t, eventTime, state.eventTime,
			"an event dated in the future must not sit in front of the events that follow it")
		assert.Equal(t, eventTime.Add(minMetricsTTL), state.expiresAt)

		metrics := collectSamplesAt(t, collector, now.Add(4*time.Minute))
		assert.NotNil(t, findMetric(metrics, osMetricName, "primary"))
		require.NotNil(t, findMetric(metrics, upMetricName, "primary"))
		assert.InDelta(t, 1.0, findMetric(metrics, upMetricName, "primary").Value, 0)
	})

	t.Run("keeps the newer sample when an older event follows it", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		collector := configuredCollector(map[instanceKey]storedSample{}, "primary")
		collector.setMetrics(futureResult(now.Add(-time.Minute)), now)
		stored := collector.metrics[testKey("primary")]

		// A scrape that read an older page and then lost the newer one to an error hands the collector
		// an event older than the one it holds. Storing it would move the instance's last event
		// backwards and bring its expiry closer, for a sample the instance has already superseded.
		collector.setMetrics(futureResult(now.Add(-2*time.Minute)), now.Add(time.Minute))

		assert.Equal(t, stored, collector.metrics[testKey("primary")],
			"an older event must not roll a sample back")
	})

	t.Run("keeps a sample dated within the accepted clock skew", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		collector := configuredCollector(map[instanceKey]storedSample{}, "primary")

		// A host a few seconds behind AWS stores every latest event dated slightly ahead of its clock.
		// That is ordinary drift, not a future dated event, so the sample is protected like any other.
		ahead := now.Add(clockSkewReportThreshold / 2)
		collector.setMetrics(futureResult(ahead), now)
		collector.setMetrics(futureResult(now.Add(-time.Minute)), now.Add(time.Minute))

		assert.Equal(t, ahead, collector.metrics[testKey("primary")].eventTime,
			"only an event dated beyond the accepted skew may be superseded by an older one")
	})

	t.Run("refuses a redelivered event after the payload is released", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		collector := configuredCollector(map[instanceKey]storedSample{}, "primary")

		// An event dated in the future is re-delivered on every scrape for as long as the account
		// keeps it: the request names no end time, and the window starts before it.
		future := now.Add(90 * time.Minute)
		collector.setMetrics(futureResult(future), now)

		released := now.Add(staleRetention + time.Minute)
		collector.setMetrics(futureResult(future), released)
		require.Nil(t, collector.metrics[testKey("primary")].metrics, "prune must have released the payload")

		later := released.Add(time.Minute)
		collector.setMetrics(futureResult(future), later)

		assert.Nil(t, collector.metrics[testKey("primary")].metrics,
			"a redelivery of the stored event says nothing new, whether or not its payload is still held")

		metrics := collectSamplesAt(t, collector, later)

		assert.Nil(t, findMetric(metrics, osMetricName, "primary"),
			"an instance that has published nothing since must not report a sample as current")
		require.NotNil(t, findMetric(metrics, upMetricName, "primary"))
		assert.InDelta(t, 0.0, findMetric(metrics, upMetricName, "primary").Value, 0,
			"health must not flap back up every retention on one event the instance never repeated")
	})

	t.Run("keeps following a clock that is behind AWS", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		collector := configuredCollector(map[instanceKey]storedSample{}, "primary")

		// Every event of every scrape is dated ahead, which is what a host an hour behind AWS sees.
		for scrape := range 3 {
			at := now.Add(time.Duration(scrape) * time.Minute)
			collector.setMetrics(futureResult(at.Add(time.Hour)), at)

			metrics := collectSamplesAt(t, collector, at)
			require.NotNil(t, findMetric(metrics, upMetricName, "primary"))
			assert.InDelta(t, 1.0, findMetric(metrics, upMetricName, "primary").Value, 0,
				"a clock that is behind AWS must not read as an instance that is down")
		}
	})

	t.Run("expires a future dated event from its receipt", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		eventTime := now.Add(90 * time.Minute)
		collector := configuredCollector(map[instanceKey]storedSample{}, "skewed")
		result := scrapeResult{
			metrics: map[instanceKey]instanceMetrics{
				testKey("skewed"): {metrics: sampleMetrics("skewed"), eventTime: eventTime},
			},
			errorCounts:  nil,
			skewedEvents: 0,
			monitored:    nil,
			region:       testRegion,
			interval:     time.Minute,
		}

		collector.setMetrics(result, now)

		// CloudWatch lets the monitored account date its own events, so expiry may not run from a
		// timestamp that has not happened yet: the entry would outlive the instance by that much.
		assert.Equal(t, now.Add(minMetricsTTL), collector.metrics[testKey("skewed")].expiresAt)

		collector.setMetrics(result, now.Add(time.Minute))

		assert.Equal(t, now.Add(minMetricsTTL), collector.metrics[testKey("skewed")].expiresAt,
			"the guard compares raw timestamps, so the re-delivered event must not renew the sample")

		metrics := collectSamplesAt(t, collector, now.Add(minMetricsTTL+time.Second))

		up := findMetric(metrics, upMetricName, "skewed")
		require.NotNil(t, up)
		assert.InDelta(t, 0.0, up.Value, 0)
	})

	t.Run("expires a sample whose event is already older than the TTL", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		collector := configuredCollector(map[instanceKey]storedSample{}, "lagging")

		collector.setMetrics(scrapeResult{
			metrics: map[instanceKey]instanceMetrics{
				testKey("lagging"): {metrics: sampleMetrics("lagging"), eventTime: now.Add(-2 * minMetricsTTL)},
			},
			errorCounts:  nil,
			skewedEvents: 0,
			monitored:    nil,
			region:       testRegion,
			interval:     time.Minute,
		}, now)

		metrics := collect(t, collector)

		// Expiry follows the event timestamp, so a clock lagging further behind than the TTL renders as a
		// gap. Reporting the sample instead would be the flat line this whole change exists to remove.
		assert.Nil(t, findMetric(metrics, osMetricName, "lagging"))

		up := findMetric(metrics, upMetricName, "lagging")
		require.NotNil(t, up)
		assert.InDelta(t, 0.0, up.Value, 0)
	})
}

func TestStop(t *testing.T) {
	t.Parallel()

	t.Run("does nothing when no scraper was started", func(t *testing.T) {
		t.Parallel()

		newCollector(promlog.New(&promlog.Config{})).Stop()
	})

	t.Run("waits for the scrapers to finish", func(t *testing.T) {
		t.Parallel()

		collector := newCollector(promlog.New(&promlog.Config{}))
		ctx, cancel := context.WithCancel(t.Context())
		collector.cancel = cancel

		finished := make(chan struct{})

		collector.wg.Go(func() {
			<-ctx.Done()
			close(finished)
		})

		collector.Stop()

		select {
		case <-finished:
		default:
			t.Fatal("Stop must not return before the scrapers have finished")
		}
	})
}

func TestPrune(t *testing.T) {
	t.Parallel()

	t.Run("keeps configured instances reported", func(t *testing.T) {
		t.Parallel()

		eventTime := time.Now().Add(-staleRetention - time.Minute)
		collector := configuredCollector(map[instanceKey]storedSample{
			testKey("down"): {
				metrics:    sampleMetrics("down"),
				eventTime:  eventTime,
				expiresAt:  time.Now().Add(-staleRetention),
				receivedAt: eventTime,
			},
		}, "down")

		collector.setMetrics(scrapeResult{
			metrics:      nil,
			errorCounts:  nil,
			skewedEvents: 0,
			monitored:    nil,
			region:       testRegion,
			interval:     time.Minute,
		}, time.Now())

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
		collector := configuredCollector(map[instanceKey]storedSample{
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
