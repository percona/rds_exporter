package basic

import (
	"context"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/percona/rds_exporter/config"
	"github.com/percona/rds_exporter/sessions"
)

var (
	Period = 60 * time.Second
	Delay  = 600 * time.Second
	Range  = 600 * time.Second
)

type Scraper struct {
	// params
	instance  *config.Instance
	resolved  *sessions.Instance
	collector *Collector
	ch        chan<- prometheus.Metric

	// internal
	svc         *cloudwatch.Client
	constLabels prometheus.Labels
}

func NewScraper(instance *config.Instance, collector *Collector, ch chan<- prometheus.Metric) *Scraper {
	cfg, resolved := collector.sessions.GetConfig(instance.Region, instance.Instance)
	if cfg == nil {
		return nil
	}
	svc := cloudwatch.NewFromConfig(*cfg)

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
		resolved:  resolved,
		collector: collector,
		ch:        ch,

		// internal
		svc:         svc,
		constLabels: constLabels,
	}
}

func getLatestDatapoint(datapoints []types.Datapoint) *types.Datapoint {
	var latest *types.Datapoint = nil
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

	end := time.Now().Add(-Delay)
	if params := clusterVolumeParams(s.resolved, end); params != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()

			if err := s.scrapeClusterVolume(params); err != nil {
				level.Error(s.collector.l).Log("metric", "VolumeBytesUsed", "error", err)
			}
		}()
	}

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

// clusterVolumeParams returns the cluster-level VolumeBytesUsed query, or nil for a non-Aurora instance.
func clusterVolumeParams(instance *sessions.Instance, end time.Time) *cloudwatch.GetMetricStatisticsInput {
	if instance == nil || !strings.HasPrefix(instance.Engine, "aurora-") || instance.ClusterID == "" {
		return nil
	}

	return &cloudwatch.GetMetricStatisticsInput{
		EndTime:    aws.Time(end),
		StartTime:  aws.Time(end.Add(-Range)),
		Period:     aws.Int32(int32(Period.Seconds())),
		MetricName: aws.String("VolumeBytesUsed"),
		Namespace:  aws.String("AWS/RDS"),
		Dimensions: []types.Dimension{{
			Name:  aws.String("DBClusterIdentifier"),
			Value: aws.String(instance.ClusterID),
		}},
		Statistics: []types.Statistic{types.StatisticAverage},
	}
}

func (s *Scraper) scrapeClusterVolume(params *cloudwatch.GetMetricStatisticsInput) error {
	resp, err := s.svc.GetMetricStatistics(context.Background(), params)
	if err != nil {
		return err
	}

	if len(resp.Datapoints) == 0 {
		return nil
	}

	labels := maps.Clone(s.constLabels)
	labels["aws_cluster_id"] = s.resolved.ClusterID
	labels["aws_account_id"] = s.resolved.AccountID

	dp := getLatestDatapoint(resp.Datapoints)
	s.ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("aws_rds_volume_bytes_used_average", "VolumeBytesUsed", nil, labels),
		prometheus.GaugeValue,
		aws.ToFloat64(dp.Average),
	)

	return nil
}

func (s *Scraper) scrapeMetric(metric Metric) error {
	now := time.Now()
	end := now.Add(-Delay)

	params := &cloudwatch.GetMetricStatisticsInput{
		EndTime:    aws.Time(end),
		StartTime:  aws.Time(end.Add(-Range)),
		Period:     aws.Int32(int32(Period.Seconds())),
		MetricName: aws.String(metric.cwName),
		Namespace:  aws.String("AWS/RDS"),
		Dimensions: []types.Dimension{{
			Name:  aws.String("DBInstanceIdentifier"),
			Value: aws.String(s.instance.Instance),
		}},
		Statistics: []types.Statistic{types.StatisticAverage},
	}

	resp, err := s.svc.GetMetricStatistics(context.Background(), params)
	if err != nil {
		return err
	}

	if len(resp.Datapoints) == 0 {
		return nil
	}

	dp := getLatestDatapoint(resp.Datapoints)
	v := aws.ToFloat64(dp.Average)
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
