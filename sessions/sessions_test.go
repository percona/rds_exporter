package sessions

import (
	"flag"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
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

	// AWS issues a new resource ID and cluster ID, and the account ID is the tester's own, so
	// this test pins only their shape.
	for _, instance := range []*Instance{am57i, p13i, m84i, ap16i} {
		require.NotNil(t, instance)
		assert.Regexp(t, `^db-[A-Z0-9]+$`, instance.ResourceID)
		assert.Regexp(t, `^\d{12}$`, instance.AccountID)
		assert.Equal(t, am57i.AccountID, instance.AccountID)
	}

	assert.NotEmpty(t, am57i.ClusterID)
	assert.NotEmpty(t, p13i.ClusterID)
	assert.Empty(t, m84i.ClusterID)
	assert.Empty(t, ap16i.ClusterID)

	am57iExpected := Instance{
		Region:                     "us-east-2",
		Instance:                   "pmm-qa-aurora3-mysql-instance-1",
		ResourceID:                 am57i.ResourceID,
		ClusterID:                  am57i.ClusterID,
		Engine:                     "aurora-mysql",
		AccountID:                  am57i.AccountID,
		EnhancedMonitoringInterval: time.Minute,
	}
	p13iExpected := Instance{
		Region:                     "us-east-2",
		Instance:                   "pmm-qa-aurora-postgres-13-5-instance-1",
		ResourceID:                 p13i.ResourceID,
		ClusterID:                  p13i.ClusterID,
		Engine:                     "aurora-postgresql",
		AccountID:                  p13i.AccountID,
		EnhancedMonitoringInterval: time.Minute,
	}
	m84iExpected := Instance{
		Region:                     "us-east-2",
		Instance:                   "pmm-qa-rds-mysql-8-4",
		ResourceID:                 m84i.ResourceID,
		ClusterID:                  m84i.ClusterID,
		Engine:                     "mysql",
		AccountID:                  m84i.AccountID,
		EnhancedMonitoringInterval: time.Minute,
	}
	ap16iExpected := Instance{
		Region:                     "us-east-2",
		Instance:                   "pmm-qa-pgsql-16",
		ResourceID:                 ap16i.ResourceID,
		ClusterID:                  ap16i.ClusterID,
		Engine:                     "postgres",
		AccountID:                  ap16i.AccountID,
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

const (
	testReplicaID = "replica"
	testPrimaryID = "primary"
	testAccountID = "123456789012"
	otherRegion   = "eu-west-1"
)

func TestUpdateInstanceStates(t *testing.T) {
	t.Parallel()

	replica := Instance{ //nolint:exhaustruct
		Region:           testRegion,
		Instance:         testReplicaID,
		ResourceID:       firstResourceID,
		Engine:           "mysql",
		AccountID:        testAccountID,
		SourceInstanceID: testPrimaryID,
		SourceRegion:     testRegion,
	}
	notDescribed := Instance{ //nolint:exhaustruct
		Region:     testRegion,
		Instance:   "not-described",
		ResourceID: secondResourceID,
		ClusterID:  "kept",
	}
	otherSession := replica
	otherSession.Region = otherRegion

	sessions := &Sessions{
		mu: sync.RWMutex{},
		sessions: map[string][]Instance{
			testRegion + "/":  {replica, notDescribed},
			otherRegion + "/": {otherSession},
		},
		Configs: map[string]aws.Config{},
	}

	promoted := dbInstance(testReplicaID, firstResourceID, nil)
	promoted.Engine = aws.String("mysql")
	promoted.DBInstanceArn = aws.String("arn:aws:rds:us-east-2:" + testAccountID + ":db:" + testReplicaID)
	client := &fakeRDSClient{pages: []*rds.DescribeDBInstancesOutput{describePage(nil, promoted)}, err: nil, calls: 0}

	states, err := (&InstanceStateResolver{svc: client, region: testRegion}).InstanceStates(t.Context())
	require.NoError(t, err)

	sessions.UpdateInstanceStates(testRegion+"/", states)

	_, got := sessions.GetConfig(testRegion, testReplicaID)
	require.NotNil(t, got)
	assert.Empty(t, got.SourceInstanceID, "a promoted read replica has no source any more")
	assert.Empty(t, got.SourceRegion)
	assert.Equal(t, testAccountID, got.AccountID)
	assert.Equal(t, firstResourceID, got.ResourceID)

	_, got = sessions.GetConfig(testRegion, "not-described")
	require.NotNil(t, got)
	assert.Equal(t, "kept", got.ClusterID, "an instance missing from the result keeps what it had")

	_, got = sessions.GetConfig(otherRegion, testReplicaID)
	require.NotNil(t, got)
	assert.Equal(t, testPrimaryID, got.SourceInstanceID, "another session's instance of the same name is not this one")

	all := sessions.AllSessions()
	all[testRegion+"/"][0].ClusterID = "changed by a caller"
	_, got = sessions.GetConfig(testRegion, testReplicaID)
	require.NotNil(t, got)
	assert.Empty(t, got.ClusterID, "AllSessions hands out a copy")
}

func TestUpdateInstanceStatesConcurrently(t *testing.T) {
	t.Parallel()

	const session = testRegion + "/"

	sessions := &Sessions{
		mu: sync.RWMutex{},
		sessions: map[string][]Instance{
			session: {{Region: testRegion, Instance: testReplicaID, ResourceID: firstResourceID}}, //nolint:exhaustruct
		},
		Configs: map[string]aws.Config{},
	}
	replica := map[string]InstanceState{testReplicaID: {SourceInstanceID: testPrimaryID, SourceRegion: testRegion}} //nolint:exhaustruct
	promoted := map[string]InstanceState{testReplicaID: {}}                                                         //nolint:exhaustruct

	var workers sync.WaitGroup

	workers.Go(func() {
		for i := range 1000 {
			if i%2 == 0 {
				sessions.UpdateInstanceStates(session, replica)
			} else {
				sessions.UpdateInstanceStates(session, promoted)
			}
		}
	})

	for range 4 {
		workers.Go(func() {
			for range 1000 {
				for _, instances := range sessions.AllSessions() {
					for _, instance := range instances {
						assert.Equal(t, instance.SourceInstanceID == "", instance.SourceRegion == "",
							"an update is seen whole or not at all")
					}
				}

				_, instance := sessions.GetConfig(testRegion, testReplicaID)
				assert.Equal(t, instance.SourceInstanceID == "", instance.SourceRegion == "")
			}
		})
	}

	workers.Wait()
}
