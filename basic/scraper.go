package basic

import (
	"context"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
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
	svc         *cloudwatch.Client
	constLabels prometheus.Labels
}

func NewScraper(instance *config.Instance, collector *Collector, ch chan<- prometheus.Metric) *Scraper {
	// Create CloudWatch client
	awsConfig, _ := collector.awsConfigs.GetSession(instance.Region, instance.Instance)
	if awsConfig == nil {
		return nil
	}
	svc := cloudwatch.NewFromConfig(*awsConfig)

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

func getLatestDatapoint(datapoints []cloudwatchtypes.Datapoint) *cloudwatchtypes.Datapoint {
	var latest *cloudwatchtypes.Datapoint = nil

	for dp := range datapoints {
		if latest == nil || latest.Timestamp.Before(*datapoints[dp].Timestamp) {
			latest = &datapoints[dp]
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
				s.collector.l.With("metric", metric.cwName).Error(err)
			}
		}()
	}
}

func (s *Scraper) scrapeMetric(metric Metric) error {
	now := time.Now()
	end := now.Add(-Delay)
	period := int32(Period.Seconds())

	params := &cloudwatch.GetMetricStatisticsInput{
		EndTime:    aws.Time(end),
		StartTime:  aws.Time(end.Add(-Range)),
		Period:     &period,
		MetricName: aws.String(metric.cwName),
		Namespace:  aws.String("AWS/RDS"),
		Dimensions: []cloudwatchtypes.Dimension{},
		Statistics: []cloudwatchtypes.Statistic{"Average"},
	}

	params.Dimensions = append(params.Dimensions, cloudwatchtypes.Dimension{
		Name:  aws.String("DBInstanceIdentifier"),
		Value: aws.String(s.instance.Instance),
	})

	// Call CloudWatch to gather the datapoints
	resp, err := s.svc.GetMetricStatistics(context.TODO(), params)
	if err != nil {
		return err
	}

	// There's nothing in there, don't publish the metric
	if len(resp.Datapoints) == 0 {
		return nil
	}

	// Pick the latest datapoint
	dp := getLatestDatapoint(resp.Datapoints)

	// Get the metric.
	v := *dp.Average
	switch metric.cwName {
	case "EngineUptime":
		// "Fake EngineUptime -> node_boot_time with time.Now().Unix() - EngineUptime."
		v = float64(time.Now().Unix() - int64(v))
	}

	// Send metric.
	s.ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(metric.prometheusName, metric.prometheusHelp, nil, s.constLabels),
		prometheus.GaugeValue,
		v,
	)

	return nil
}
