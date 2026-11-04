package basic

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/percona/rds_exporter/sessions"
)

func TestClusterVolumeParams(t *testing.T) {
	t.Parallel()

	end := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	params := clusterVolumeParams(&sessions.Instance{Engine: "aurora-mysql", ClusterID: "cluster-a"}, end)
	require.NotNil(t, params)
	assert.Equal(t, "VolumeBytesUsed", aws.ToString(params.MetricName))
	assert.Equal(t, "AWS/RDS", aws.ToString(params.Namespace))
	assert.Equal(t, []types.Dimension{{
		Name:  aws.String("DBClusterIdentifier"),
		Value: aws.String("cluster-a"),
	}}, params.Dimensions)
	assert.Equal(t, []types.Statistic{types.StatisticAverage}, params.Statistics)
	assert.Equal(t, int32(60), aws.ToInt32(params.Period))
	assert.True(t, end.Equal(aws.ToTime(params.EndTime)))
	assert.True(t, end.Add(-Range).Equal(aws.ToTime(params.StartTime)))

	assert.Nil(t, clusterVolumeParams(&sessions.Instance{Engine: "aurora-postgresql"}, end))
	assert.Nil(t, clusterVolumeParams(&sessions.Instance{Engine: "mysql", ClusterID: "x"}, end))
}
