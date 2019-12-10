package sessions

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/percona/rds_exporter/client"
	"github.com/percona/rds_exporter/config"
)

func TestSession(t *testing.T) {
	cfg, err := config.Load("../config.tests.yml")
	require.NoError(t, err)

	// set explicit keys to test grouping
	cfg.Instances[1].AWSAccessKey = os.Getenv("AWS_ACCESS_KEY")
	cfg.Instances[1].AWSSecretKey = os.Getenv("AWS_SECRET_KEY")

	client := client.New()
	sessions, err := New(cfg.Instances, client.HTTP(), false)
	require.NoError(t, err)

	a1s, a1i := sessions.GetSession("us-east-1", "rds-aurora1")
	a57s, a57i := sessions.GetSession("us-east-1", "rds-aurora57")
	m56s, m56i := sessions.GetSession("us-east-1", "rds-mysql56")
	m57s, m57i := sessions.GetSession("us-west-1", "rds-mysql57")

	if a1s == a57s {
		assert.Fail(t, "rds-aurora1 and rds-aurora57 should not share session (different keys)")
	}
	if a1s != m56s {
		assert.Fail(t, "rds-aurora1 and rds-mysql56 should share session")
	}
	if m57s != nil {
		assert.Fail(t, "rds-mysql57 should be skipped")
	}

	assert.Equal(t, &Instance{
		Region:                     "us-east-1",
		Instance:                   "rds-aurora1",
		ResourceID:                 "db-6KU3QFZVR6GGRRYAKU6F7RORAA",
		EnhancedMonitoringInterval: time.Minute,
	}, a1i)
	assert.Nil(t, a57i)
	assert.Equal(t, &Instance{
		Region:                     "us-east-1",
		Instance:                   "rds-mysql56",
		ResourceID:                 "db-QUE57ZGTLVEIMZWB4BXOYMYF6A",
		EnhancedMonitoringInterval: time.Minute,
	}, m56i)
	assert.Nil(t, m57i)

	// TODO
	// all := sessions.AllSessions()
	// assert.Equal(t, map[*session.Session][]Instance{
	// 	a1s: {
	// 		{
	// 			Region:                     "us-east-1",
	// 			Instance:                   "rds-aurora1",
	// 			ResourceID:                 "db-6KU3QFZVR6GGRRYAKU6F7RORAA",
	// 			EnhancedMonitoringInterval: time.Minute,
	// 		},
	// 		{
	// 			Region:                     "us-east-1",
	// 			Instance:                   "rds-mysql56",
	// 			ResourceID:                 "db-QUE57ZGTLVEIMZWB4BXOYMYF6A",
	// 			EnhancedMonitoringInterval: time.Minute,
	// 		},
	// 	},
	// 	a57s: {
	// 		{
	// 			Region:                     "us-east-1",
	// 			Instance:                   "rds-aurora57",
	// 			ResourceID:                 "db-CDBSN4EK5SMBQCSXI4UPZVF3W4",
	// 			EnhancedMonitoringInterval: time.Minute,
	// 		},
	// 	},
	// }, all)
}
