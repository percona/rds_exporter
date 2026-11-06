package basic

import (
	"maps"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/percona/exporter_shared/helpers"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/percona/rds_exporter/sessions"
)

const (
	testClusterID = "cluster-a"
	labelRegion   = "region"
	labelInstance = "instance"
)

func TestClusterVolumeParams(t *testing.T) {
	t.Parallel()

	end := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	params := clusterVolumeParams(&sessions.Instance{Engine: "aurora-mysql", ClusterID: testClusterID}, end) //nolint:exhaustruct
	require.NotNil(t, params)
	assert.Equal(t, "VolumeBytesUsed", aws.ToString(params.MetricName))
	assert.Equal(t, "AWS/RDS", aws.ToString(params.Namespace))
	assert.Equal(t, []types.Dimension{{
		Name:  aws.String("DBClusterIdentifier"),
		Value: aws.String(testClusterID),
	}}, params.Dimensions)
	assert.Equal(t, []types.Statistic{types.StatisticAverage}, params.Statistics)
	assert.Equal(t, int32(60), aws.ToInt32(params.Period))
	assert.True(t, end.Equal(aws.ToTime(params.EndTime)))
	assert.True(t, end.Add(-clusterVolumeRange).Equal(aws.ToTime(params.StartTime)))

	assert.NotNil(t, clusterVolumeParams(&sessions.Instance{Engine: "aurora", ClusterID: "x"}, end)) //nolint:exhaustruct
	assert.Nil(t, clusterVolumeParams(&sessions.Instance{Engine: "aurora-postgresql"}, end))         //nolint:exhaustruct
	assert.Nil(t, clusterVolumeParams(&sessions.Instance{Engine: "mysql", ClusterID: "x"}, end))     //nolint:exhaustruct
}

func TestClusterVolumeMetric(t *testing.T) {
	t.Parallel()

	instance := &sessions.Instance{ClusterID: testClusterID, AccountID: "123456789012"} //nolint:exhaustruct
	constLabels := prometheus.Labels{labelRegion: "us-east-2", labelInstance: "a3-instance-1", "node_name": "a3"}
	sharedLabels := maps.Clone(constLabels)
	older := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

	metric := clusterVolumeMetric([]types.Datapoint{
		{Timestamp: aws.Time(older.Add(time.Hour)), Average: aws.Float64(2048)}, //nolint:exhaustruct
		{Timestamp: aws.Time(older), Average: aws.Float64(1024)},                //nolint:exhaustruct
	}, constLabels, instance)
	require.NotNil(t, metric)

	got := helpers.ReadMetric(metric)
	assert.Equal(t, "aws_rds_volume_bytes_used_average", got.Name)
	assert.Equal(t, prometheus.Labels{
		labelRegion:      "us-east-2",
		labelInstance:    "a3-instance-1",
		"node_name":      "a3",
		"aws_cluster_id": testClusterID,
		"aws_account_id": "123456789012",
	}, got.Labels)
	assert.InDelta(t, 2048, got.Value, 0, "the latest datapoint wins, wherever it is in the response")
	assert.Equal(t, sharedLabels, constLabels, "the instance labels are shared by every series of the scrape")

	assert.Nil(t, clusterVolumeMetric(nil, constLabels, instance))
}

func TestMetricsFor(t *testing.T) {
	t.Parallel()

	var mysqlOnly int
	for _, m := range Metrics {
		if m.mysqlOnly {
			mysqlOnly++
		}
	}
	require.Len(t, Metrics, 47)
	require.Equal(t, 19, mysqlOnly)

	for _, engine := range []string{"postgres", "aurora-postgresql"} {
		t.Run(engine, func(t *testing.T) {
			t.Parallel()

			metrics := metricsFor(engine, Metrics)
			assert.Len(t, metrics, 28)
			for _, m := range metrics {
				assert.False(t, m.mysqlOnly, m.cwName)
			}
		})
	}

	// An empty engine means the AWS lookup failed or has not run yet: request everything, as before.
	for _, engine := range []string{"aurora-mysql", "mysql", "mariadb", "aurora", ""} {
		t.Run("engine "+engine, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, Metrics, metricsFor(engine, Metrics))
		})
	}
}
