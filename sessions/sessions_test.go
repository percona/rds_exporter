package sessions

import (
	"flag"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws/session"
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

	client := client.New()
	sessions, err := New(cfg.Instances, client.HTTP(), false)
	require.NoError(t, err)

	am56s, am56i := sessions.GetSession("us-east-1", "autotest-aurora-mysql-56")
	p10s, p10i := sessions.GetSession("us-east-1", "autotest-psql-10")
	m57s, m57i := sessions.GetSession("us-west-2", "autotest-mysql-57")
	ap11s, ap11i := sessions.GetSession("us-west-2", "autotest-aurora-psql-11")
	ns, ni := sessions.GetSession("us-west-2", "no-such-instance")

	if am56s == p10s {
		assert.Fail(t, "autotest-aurora-mysql-56 and autotest-psql-10 should not share session - different keys (implicit and explicit)")
	}
	if p10s == m57s {
		assert.Fail(t, "autotest-psql-10 and autotest-mysql-57 should not share session - different regions")
	}
	if m57s != ap11s {
		assert.Fail(t, "autotest-mysql-57 and autotest-aurora-psql-11 should share session")
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
		ResourceID:                 "db-OZNCI2RJ7VU3IE3XE52ZCLVBMA",
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
		ResourceID:                 "db-MZ2RNFOFFZTGHAE2QR46PD2CH4",
		EnhancedMonitoringInterval: time.Minute,
	}

	assert.Equal(t, &am56iExpected, am56i)
	assert.Equal(t, &p10iExpected, p10i)
	assert.Equal(t, &m57iExpected, m57i)
	assert.Equal(t, &ap11iExpected, ap11i)
	assert.Nil(t, ni)

	all := sessions.AllSessions()
	assert.Equal(t, map[*session.Session][]Instance{
		am56s: {am56iExpected},
		p10s:  {p10iExpected},
		m57s:  {m57iExpected, ap11iExpected},
		// ap11s == m57s
	}, all)
}
