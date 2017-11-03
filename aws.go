package main

import (
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/prometheus/client_golang/prometheus"
)

func getLatestDatapoint(datapoints []*cloudwatch.Datapoint) *cloudwatch.Datapoint {
	var latest *cloudwatch.Datapoint = nil

	for dp := range datapoints {
		if latest == nil || latest.Timestamp.Before(*datapoints[dp].Timestamp) {
			latest = datapoints[dp]
		}
	}

	return latest
}

// scrape makes the required calls to AWS CloudWatch by using the parameters in the cwCollector
// Once converted into Prometheus format, the metrics are pushed on the ch channel.
func scrape(collector *cwCollector, ch chan<- prometheus.Metric) {
	session := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(collector.Region),
	}))

	svc := cloudwatch.New(session)

	for m := range collector.Template.Metrics {
		metric := &collector.Template.Metrics[m]

		now := time.Now()
		end := now.Add(time.Duration(-metric.ConfMetric.DelaySeconds) * time.Second)

		params := &cloudwatch.GetMetricStatisticsInput{
			EndTime:   aws.Time(end),
			StartTime: aws.Time(end.Add(time.Duration(-metric.ConfMetric.RangeSeconds) * time.Second)),

			Period:     aws.Int64(int64(metric.ConfMetric.PeriodSeconds)),
			MetricName: aws.String(metric.ConfMetric.Name),
			Namespace:  aws.String(metric.ConfMetric.Namespace),
			Dimensions: []*cloudwatch.Dimension{},
			Statistics: []*string{},
			Unit:       nil,
		}

		for _, stat := range metric.ConfMetric.Statistics {
			params.Statistics = append(params.Statistics, aws.String(stat))
		}

		labels := make([]string, 0, len(metric.LabelNames))

		// Loop through the dimensions selects to build the filters and the labels array
		for dim := range metric.ConfMetric.DimensionsSelect {
			for val := range metric.ConfMetric.DimensionsSelect[dim] {
				dimValue := metric.ConfMetric.DimensionsSelect[dim][val]

				// Replace $_target token by the actual URL target
				if dimValue == "$_target" {
					dimValue = collector.Target
				}

				params.Dimensions = append(params.Dimensions, &cloudwatch.Dimension{
					Name:  aws.String(dim),
					Value: aws.String(dimValue),
				})

				labels = append(labels, dimValue)
			}
		}

		labels = append(labels, collector.Template.Task.Name)

		// Call CloudWatch to gather the datapoints
		resp, err := svc.GetMetricStatistics(params)
		totalRequests.Inc()

		if err != nil {
			collector.ErroneousRequests.Inc()
			fmt.Println(err)
			continue
		}

		// There's nothing in there, don't publish the metric
		if len(resp.Datapoints) == 0 {
			continue
		}

		// Pick the latest datapoint
		dp := getLatestDatapoint(resp.Datapoints)
		if dp.Sum != nil {
			ch <- prometheus.MustNewConstMetric(metric.Desc, metric.ValType, float64(*dp.Sum), labels...)
		}

		if dp.Average != nil {
			ch <- prometheus.MustNewConstMetric(metric.Desc, metric.ValType, float64(*dp.Average), labels...)
		}

		if dp.Maximum != nil {
			ch <- prometheus.MustNewConstMetric(metric.Desc, metric.ValType, float64(*dp.Maximum), labels...)
		}

		if dp.Minimum != nil {
			ch <- prometheus.MustNewConstMetric(metric.Desc, metric.ValType, float64(*dp.Minimum), labels...)
		}

		if dp.SampleCount != nil {
			ch <- prometheus.MustNewConstMetric(metric.Desc, metric.ValType, float64(*dp.SampleCount), labels...)
		}
	}
}
