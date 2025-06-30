package basic

import (
	"context"
	"sync"
	"time"

	awsV2 "github.com/aws/aws-sdk-go-v2/aws"
	cloudwatchV2 "github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchV2types "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/percona/rds_exporter/config"
)

var (
	Period = 60 * time.Second
	Delay  = 600 * time.Second
	Range  = 600 * time.Second
)

type Scraper struct {
	// params
	instance  *config.Instance
	collector *Collector
	ch        chan<- prometheus.Metric

	// internal
	svc         *cloudwatchV2.Client
	constLabels prometheus.Labels
}

func NewScraper(instance *config.Instance, collector *Collector, ch chan<- prometheus.Metric) *Scraper {
	cfg, _ := collector.sessions.GetSession(instance.Region, instance.Instance)
	if cfg == nil {
		return nil
	}
	svc := cloudwatchV2.NewFromConfig(*cfg)

	constLabels := prometheus.Labels{
		"region":   instance.Region,
		"instance": instance.Instance,
	}
	for n, v := range instance.Labels {
		if v == "" {
			delete(constLabels, n)
		} else {
			constLabels[n] = v
		}
	}

	return &Scraper{
		// params
		instance:  instance,
		collector: collector,
		ch:        ch,

		// internal
		svc:         svc,
		constLabels: constLabels,
	}
}

func getLatestDatapoint(datapoints []cloudwatchV2types.Datapoint) *cloudwatchV2types.Datapoint {
	var latest *cloudwatchV2types.Datapoint = nil
	for i := range datapoints {
		if latest == nil || latest.Timestamp.Before(*datapoints[i].Timestamp) {
			latest = &datapoints[i]
		}
	}
	return latest
}

// Scrape makes the required calls to AWS CloudWatch by using the parameters in the Collector.
// Once converted into Prometheus format, the metrics are pushed on the ch channel.
func (s *Scraper) Scrape() {
	var wg sync.WaitGroup
	defer wg.Wait()

	wg.Add(len(s.collector.metrics))
	for _, metric := range s.collector.metrics {
		metric := metric
		go func() {
			defer wg.Done()

			if err := s.scrapeMetric(metric); err != nil {
				level.Error(s.collector.l).Log("metric", metric.cwName, "error", err)
			}
		}()
	}
}

func (s *Scraper) scrapeMetric(metric Metric) error {
	now := time.Now()
	end := now.Add(-Delay)

	params := &cloudwatchV2.GetMetricStatisticsInput{
		EndTime:    awsV2.Time(end),
		StartTime:  awsV2.Time(end.Add(-Range)),
		Period:     awsV2.Int32(int32(Period.Seconds())),
		MetricName: awsV2.String(metric.cwName),
		Namespace:  awsV2.String("AWS/RDS"),
		Dimensions: []cloudwatchV2types.Dimension{{
			Name:  awsV2.String("DBInstanceIdentifier"),
			Value: awsV2.String(s.instance.Instance),
		}},
		Statistics: []cloudwatchV2types.Statistic{cloudwatchV2types.StatisticAverage},
	}

	resp, err := s.svc.GetMetricStatistics(context.Background(), params)
	if err != nil {
		return err
	}

	if len(resp.Datapoints) == 0 {
		return nil
	}

	dp := getLatestDatapoint(resp.Datapoints)
	v := awsV2.ToFloat64(dp.Average)
	switch metric.cwName {
	case "EngineUptime":
		v = float64(time.Now().Unix() - int64(v))
	}

	s.ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(metric.prometheusName, metric.prometheusHelp, nil, s.constLabels),
		prometheus.GaugeValue,
		v,
	)

	return nil
}
