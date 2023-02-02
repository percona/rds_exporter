package enhanced

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/percona/rds_exporter/sessions"
)

// scraper retrieves metrics from several RDS instances sharing a single session.
type scraper struct {
	instances      []sessions.Instance
	logStreamNames []string
	svc            *cloudwatchlogs.CloudWatchLogs
	nextStartTime  time.Time
	logger         log.Logger

	testDisallowUnknownFields bool // for tests only
}

func newScraper(session *session.Session, instances []sessions.Instance, logger log.Logger) *scraper {
	logStreamNames := make([]string, 0, len(instances))
	for _, instance := range instances {
		logStreamNames = append(logStreamNames, instance.ResourceID)
	}

	return &scraper{
		instances:      instances,
		logStreamNames: logStreamNames,
		svc:            cloudwatchlogs.New(session),
		nextStartTime:  time.Now().Add(-3 * time.Minute).Round(0), // strip monotonic clock reading
		logger:         log.With(logger, "component", "enhanced"),
	}
}

// start scrapes metrics in loop and sends them to the channel until context is canceled.
func (s *scraper) start(ctx context.Context, interval time.Duration, ch chan<- map[string][]prometheus.Metric) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// nothing
		case <-ctx.Done():
			return
		}

		scrapeCtx, cancel := context.WithTimeout(ctx, interval)
		m, _ := s.scrape(scrapeCtx)
		cancel()
		ch <- m
	}
}

// scrape performs a single scrape.
func (s *scraper) scrape(ctx context.Context) (map[string][]prometheus.Metric, map[string]string) {

	allMetrics := make(map[string]map[time.Time][]prometheus.Metric) // ResourceID -> event timestamp -> metrics
	allMessages := make(map[string]map[time.Time]string)             // ResourceID -> event timestamp -> message

	// LogStreamNames parameter supports up to 100 items.
	// https://docs.aws.amazon.com/AmazonCloudWatchLogs/latest/APIReference/API_FilterLogEvents.html
	streamCount := len(s.logStreamNames)
	for i := 0; i < streamCount; i += 100 {
		sliceStart := i
		sliceEnd := i + 100
		if sliceEnd > streamCount {
			sliceEnd = streamCount
		}

		input := &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName:   aws.String("RDSOSMetrics"),
			LogStreamNames: aws.StringSlice(s.logStreamNames[sliceStart:sliceEnd]),
			StartTime:      aws.Int64(aws.TimeUnixMilli(s.nextStartTime)),
		}

		level.Debug(log.With(s.logger, "next_start", s.nextStartTime.UTC(), "since_last", time.Since(s.nextStartTime))).Log("msg", "Requesting metrics")

		// collect all returned events and metrics/messages
		collectAllMetrics := func(output *cloudwatchlogs.FilterLogEventsOutput, lastPage bool) bool {
			for _, event := range output.Events {
				l := log.With(s.logger,
					"EventId", *event.EventId,
					"LogStreamName", *event.LogStreamName,
					"Timestamp", aws.MillisecondsTimeValue(event.Timestamp).UTC(),
					"IngestionTime", aws.MillisecondsTimeValue(event.IngestionTime).UTC())

				var instance *sessions.Instance
				for _, i := range s.instances {
					if i.ResourceID == *event.LogStreamName {
						instance = &i
						break
					}
				}
				if instance == nil {
					level.Error(l).Log("msg", "Failed to find instance.")
					continue
				}

				if instance.DisableEnhancedMetrics {
					level.Debug(l).Log("msg", fmt.Sprintf("Enhanced Metrics are disabled for instance %v.", instance))
					continue
				}
				l = log.With(l, "region", instance.Region, "instance", instance.Instance)

				// l.Debugf("Message:\n%s", *event.Message)
				osMetrics, err := parseOSMetrics([]byte(*event.Message), s.testDisallowUnknownFields)
				if err != nil {
					// only for tests
					if s.testDisallowUnknownFields {
						panic(fmt.Sprintf("New metrics should be added: %s", err))
					}

					level.Error(l).Log("msg", "Failed to parse metrics.", "error", err)
					continue
				}
				// l.Debugf("OS Metrics:\n%#v", osMetrics)

				timestamp := aws.MillisecondsTimeValue(event.Timestamp).UTC()
				level.Debug(l).Log("msg", fmt.Sprintf("Timestamp from message: %s; from event: %s.", osMetrics.Timestamp.UTC(), timestamp))

				if allMetrics[instance.ResourceID] == nil {
					allMetrics[instance.ResourceID] = make(map[time.Time][]prometheus.Metric)
				}
				allMetrics[instance.ResourceID][timestamp] = osMetrics.makePrometheusMetrics(instance.Region, instance.Labels)

				if allMessages[instance.ResourceID] == nil {
					allMessages[instance.ResourceID] = make(map[time.Time]string)
				}
				allMessages[instance.ResourceID][timestamp] = *event.Message
			}

			return true // continue pagination
		}
		if err := s.svc.FilterLogEventsPagesWithContext(ctx, input, collectAllMetrics); err != nil {
			level.Error(s.logger).Log("msg", "Failed to filter log events.", "error", err)
		}
	}
	// get better times
	allTimes := make(map[string][]time.Time)
	for resourceID, events := range allMetrics {
		allTimes[resourceID] = make([]time.Time, 0, len(events))
		for timestamp := range events {
			allTimes[resourceID] = append(allTimes[resourceID], timestamp)
		}
	}
	var times map[string]time.Time
	times, s.nextStartTime = betterTimes(allTimes)

	// return only latest metrics/messages
	resMetrics := make(map[string][]prometheus.Metric) // ResourceID -> metrics
	resMessages := make(map[string]string)             // ResourceID -> message
	for resourceID, timestamp := range times {
		resMetrics[resourceID] = allMetrics[resourceID][timestamp]
		resMessages[resourceID] = allMessages[resourceID][timestamp]
	}
	return resMetrics, resMessages
}

// betterTimes returns timestamps of the latest metrics, and also StarTime that should be used in the next request
func betterTimes(allTimes map[string][]time.Time) (times map[string]time.Time, nextStartTime time.Time) {
	// keep only the most recent metrics for each instance
	nextStartTime = time.Now()
	times = make(map[string]time.Time) // ResourceID -> timestamp
	for resourceID, events := range allTimes {
		var newest time.Time
		for _, timestamp := range events {
			if newest.Before(timestamp) {
				newest = timestamp
				times[resourceID] = timestamp
			}
		}

		if nextStartTime.After(newest) {
			nextStartTime = newest
		}
	}

	return
}
