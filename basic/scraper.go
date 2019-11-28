package basic

import (
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
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
	instance *config.Instance
	exporter *Exporter
	ch       chan<- prometheus.Metric

	// internal
	svc    *cloudwatch.CloudWatch
	labels []string
}

func NewScraper(instance *config.Instance, exporter *Exporter, ch chan<- prometheus.Metric) *Scraper {
	// Create CloudWatch client
	sess, _ := exporter.sessions.GetSession(instance.Region, instance.Instance)
	if sess == nil {
		return nil
	}
	svc := cloudwatch.New(sess)

	// Create labels for all metrics
	labels := []string{
		instance.Instance,
		instance.Region,
	}

	return &Scraper{
		// params
		instance: instance,
		exporter: exporter,
		ch:       ch,

		// internal
		svc:    svc,
		labels: labels,
	}
}

func getLatestDatapoint(datapoints []*cloudwatch.Datapoint) *cloudwatch.Datapoint {
	var latest *cloudwatch.Datapoint = nil

	for dp := range datapoints {
		if latest == nil || latest.Timestamp.Before(*datapoints[dp].Timestamp) {
			latest = datapoints[dp]
		}
	}

	return latest
}

// Scrape makes the required calls to AWS CloudWatch by using the parameters in the Exporter.
// Once converted into Prometheus format, the metrics are pushed on the ch channel.
func (s *Scraper) Scrape() {
	wg := &sync.WaitGroup{}
	defer wg.Wait()

	wg.Add(len(s.exporter.metrics))
	for _, metric := range s.exporter.metrics {
		go func(metric Metric) {
			err := s.scrapeMetric(metric)
			if err != nil {
				s.exporter.l.With("metric", metric.Name).Error(err)
			}

			wg.Done()
		}(metric)
	}
}

func (s *Scraper) scrapeMetric(metric Metric) error {
	now := time.Now()
	end := now.Add(-Delay)

	params := &cloudwatch.GetMetricStatisticsInput{
		EndTime:   aws.Time(end),
		StartTime: aws.Time(end.Add(-Range)),

		Period:     aws.Int64(int64(Period.Seconds())),
		MetricName: aws.String(metric.Name),
		Namespace:  aws.String("AWS/RDS"),
		Dimensions: []*cloudwatch.Dimension{},
		Statistics: aws.StringSlice([]string{"Average"}),
		Unit:       nil,
	}

	params.Dimensions = append(params.Dimensions, &cloudwatch.Dimension{
		Name:  aws.String("DBInstanceIdentifier"),
		Value: aws.String(s.instance.Instance),
	})

	// Call CloudWatch to gather the datapoints
	resp, err := s.svc.GetMetricStatistics(params)
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
	v := aws.Float64Value(dp.Average)
	switch metric.Name {
	case "EngineUptime":
		// "Fake EngineUptime -> node_boot_time with time.Now().Unix() - EngineUptime."
		v = float64(time.Now().Unix() - int64(v))
	}

	// Send metric.
	s.ch <- prometheus.MustNewConstMetric(metric.Desc, prometheus.GaugeValue, v, s.labels...)

	return nil
}
