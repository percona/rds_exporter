package sessions

import (
	"flag"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/common/promlog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/percona/rds_exporter/client"
	"github.com/percona/rds_exporter/config"
)

var (
	golden    = flag.Bool("golden", false, "does nothing; exists only for compatibility with other packages")
	goldenTXT = flag.Bool("golden-txt", false, "does nothing; exists only for compatibility with other packages")
)

func TestSession(t *testing.T) {
	cfg, err := config.Load("../config.tests.yml")
	require.NoError(t, err)

	// set explicit keys to first instance to test grouping
	cfg.Instances[0].AWSAccessKey = os.Getenv("AWS_ACCESS_KEY")
	cfg.Instances[0].AWSSecretKey = os.Getenv("AWS_SECRET_KEY")
	if cfg.Instances[0].AWSAccessKey == "" || cfg.Instances[0].AWSSecretKey == "" {
		require.Fail(t, "AWS_ACCESS_KEY and AWS_SECRET_KEY environment variables must be set for this test")
	}

	logger := promlog.New(&promlog.Config{})
	client := client.New(logger)
	sessions, err := New(cfg.Instances, client.HTTP(), logger, false)
	require.NoError(t, err)

	am57s, am57i := sessions.GetConfig("us-east-2", "pmm-qa-aurora3-mysql-instance-1")
	p13s, p13i := sessions.GetConfig("us-east-2", "pmm-qa-aurora-postgres-13-5-instance-1")
	m84s, m84i := sessions.GetConfig("us-east-2", "pmm-qa-rds-mysql-8-4")
	ap16s, ap16i := sessions.GetConfig("us-east-2", "pmm-qa-pgsql-16")
	ns, ni := sessions.GetConfig("us-east-2", "no-such-instance")

	if reflect.DeepEqual(am57s, p13s) {
		assert.Fail(t, "pmm-qa-aurora3-mysql-instance-1 and pmm-qa-aurora-postgres-13-5-instance-1 should not share config - different keys (implicit and explicit)")
	}
	if !reflect.DeepEqual(p13s, m84s) {
		assert.Fail(t, "pmm-qa-aurora-postgres-13-5-instance-1 and pmm-qa-rds-mysql-8-4 should share config - same regions")
	}
	if !reflect.DeepEqual(m84s, ap16s) {
		assert.Fail(t, "pmm-qa-rds-mysql-8-4 and pmm-qa-pgsql-16 should share config")
	}
	if ns != nil {
		assert.Fail(t, "no-such-instance does not exist")
	}

	am57iExpected := Instance{
		Region:                     "us-east-2",
		Instance:                   "pmm-qa-aurora3-mysql-instance-1",
		ResourceID:                 "db-DFQSTXQUYKPPAFNHGTJ27P5PTU",
		EnhancedMonitoringInterval: time.Minute,
	}
	p13iExpected := Instance{
		Region:                     "us-east-2",
		Instance:                   "pmm-qa-aurora-postgres-13-5-instance-1",
		ResourceID:                 "db-ZUYAKAIMBFOPJOW3I5YKUI3XXY",
		EnhancedMonitoringInterval: time.Minute,
	}
	m84iExpected := Instance{
		Region:                     "us-east-2",
		Instance:                   "pmm-qa-rds-mysql-8-4",
		ResourceID:                 "db-POWENXD5M5PQ34OZFZKSUPNX64",
		EnhancedMonitoringInterval: time.Minute,
	}
	ap16iExpected := Instance{
		Region:                     "us-east-2",
		Instance:                   "pmm-qa-pgsql-16",
		ResourceID:                 "db-H5NM4CLYE5MF4DSEIZAX2BSWNE",
		EnhancedMonitoringInterval: time.Minute,
	}

	assert.Equal(t, &am57iExpected, am57i)
	assert.Equal(t, &p13iExpected, p13i)
	assert.Equal(t, &m84iExpected, m84i)
	assert.Equal(t, &ap16iExpected, ap16i)
	assert.Nil(t, ni)

	all := sessions.AllSessions()
	assert.Equal(t, map[string][]Instance{
		"us-east-2/" + os.Getenv("AWS_ACCESS_KEY"): {am57iExpected},
		"us-east-2/": {p13iExpected, m84iExpected, ap16iExpected},
	}, all)
}
