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

	am56s, am56i := sessions.GetConfig("us-east-1", "autotest-aurora-mysql-56")
	p10s, p10i := sessions.GetConfig("us-east-1", "autotest-psql-10")
	m57s, m57i := sessions.GetConfig("us-west-2", "autotest-mysql-57")
	ap11s, ap11i := sessions.GetConfig("us-west-2", "autotest-aurora-psql-11")
	ns, ni := sessions.GetConfig("us-west-2", "no-such-instance")

	if reflect.DeepEqual(am56s, p10s) {
		assert.Fail(t, "autotest-aurora-mysql-56 and autotest-psql-10 should not share config - different keys (implicit and explicit)")
	}
	if reflect.DeepEqual(p10s, m57s) {
		assert.Fail(t, "autotest-psql-10 and autotest-mysql-57 should not share config - different regions")
	}
	if !reflect.DeepEqual(m57s, ap11s) {
		assert.Fail(t, "autotest-mysql-57 and autotest-aurora-psql-11 should share config")
	}
	if ns != nil {
		assert.Fail(t, "no-such-instance does not exist")
	}

	am56iExpected := Instance{
		Region:                     "us-east-1",
		Instance:                   "autotest-aurora-mysql-56",
		ResourceID:                 "db-OQT42DPIZWWQBVXQ2LH2BW3SV4",
		EnhancedMonitoringInterval: time.Minute,
	}
	p10iExpected := Instance{
		Region:                     "us-east-1",
		Instance:                   "autotest-psql-10",
		ResourceID:                 "db-PUZFCRUUHY365QFJLTOUWRDOCQ",
		EnhancedMonitoringInterval: time.Minute,
	}
	m57iExpected := Instance{
		Region:                     "us-west-2",
		Instance:                   "autotest-mysql-57",
		ResourceID:                 "db-QXZYJIL5GR3CBQ4XNCYU2AI5PE",
		EnhancedMonitoringInterval: time.Minute,
	}
	ap11iExpected := Instance{
		Region:                     "us-west-2",
		Instance:                   "autotest-aurora-psql-11",
		ResourceID:                 "db-TYM5GWPPEMFCR5L6YX6ZBHUIUE",
		EnhancedMonitoringInterval: time.Minute,
	}

	assert.Equal(t, &am56iExpected, am56i)
	assert.Equal(t, &p10iExpected, p10i)
	assert.Equal(t, &m57iExpected, m57i)
	assert.Equal(t, &ap11iExpected, ap11i)
	assert.Nil(t, ni)

	all := sessions.AllSessions()
	assert.Equal(t, map[string][]Instance{
		"us-east-1/" + os.Getenv("AWS_ACCESS_KEY"): {am56iExpected},
		"us-east-1/": {p10iExpected},
		"us-west-2/": {m57iExpected, ap11iExpected},
	}, all)
}
