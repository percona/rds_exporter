package sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	firstResourceID  = "db-1"
	secondResourceID = "db-2"
)

var errDescribeRefused = errors.New("describe refused")

// fakeRDSClient replays canned DescribeDBInstances pages.
type fakeRDSClient struct {
	pages []*rds.DescribeDBInstancesOutput
	err   error

	calls int
}

func (c *fakeRDSClient) DescribeDBInstances(
	_ context.Context,
	_ *rds.DescribeDBInstancesInput,
	_ ...func(*rds.Options),
) (*rds.DescribeDBInstancesOutput, error) {
	if c.calls >= len(c.pages) {
		return nil, c.err
	}

	page := c.pages[c.calls]
	c.calls++

	return page, nil
}

func dbInstance(id, resourceID string, monitoringInterval *int32) types.DBInstance {
	return types.DBInstance{ //nolint:exhaustruct
		DBInstanceIdentifier: aws.String(id),
		DbiResourceId:        aws.String(resourceID),
		MonitoringInterval:   monitoringInterval,
	}
}

func describePage(marker *string, instances ...types.DBInstance) *rds.DescribeDBInstancesOutput {
	return &rds.DescribeDBInstancesOutput{ //nolint:exhaustruct
		DBInstances: instances,
		Marker:      marker,
	}
}

type instanceStatesTestCase struct {
	name           string
	pages          []*rds.DescribeDBInstancesOutput
	expectedStates map[string]InstanceState
	expectedCalls  int
}

func instanceStatesTestCases() []instanceStatesTestCase {
	return []instanceStatesTestCase{
		{
			name: "reports the monitoring interval alongside the resource ID",
			pages: []*rds.DescribeDBInstancesOutput{describePage(nil,
				dbInstance("monitored", firstResourceID, aws.Int32(60)),
				dbInstance("unmonitored", secondResourceID, aws.Int32(0)),
			)},
			expectedStates: map[string]InstanceState{
				"monitored":   {ResourceID: firstResourceID, MonitoringInterval: time.Minute},
				"unmonitored": {ResourceID: secondResourceID, MonitoringInterval: 0},
			},
			expectedCalls: 1,
		},
		{
			name:  "treats an absent monitoring interval as disabled",
			pages: []*rds.DescribeDBInstancesOutput{describePage(nil, dbInstance("no-interval", firstResourceID, nil))},
			expectedStates: map[string]InstanceState{
				"no-interval": {ResourceID: firstResourceID, MonitoringInterval: 0},
			},
			expectedCalls: 1,
		},
		{
			name: "collects every page",
			pages: []*rds.DescribeDBInstancesOutput{
				describePage(aws.String("next"), dbInstance("first", firstResourceID, aws.Int32(1))),
				describePage(nil, dbInstance("second", secondResourceID, aws.Int32(5))),
			},
			expectedStates: map[string]InstanceState{
				"first":  {ResourceID: firstResourceID, MonitoringInterval: time.Second},
				"second": {ResourceID: secondResourceID, MonitoringInterval: 5 * time.Second},
			},
			expectedCalls: 2,
		},
		{
			name: "skips instances without a resource ID",
			pages: []*rds.DescribeDBInstancesOutput{describePage(nil,
				dbInstance("usable", firstResourceID, aws.Int32(60)),
				dbInstance("creating", "", aws.Int32(60)),
			)},
			expectedStates: map[string]InstanceState{
				"usable": {ResourceID: firstResourceID, MonitoringInterval: time.Minute},
			},
			expectedCalls: 1,
		},
	}
}

func TestInstanceStates(t *testing.T) {
	t.Parallel()

	for _, testCase := range instanceStatesTestCases() {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			client := &fakeRDSClient{pages: testCase.pages, err: nil, calls: 0}

			states, err := (&ResourceIDResolver{svc: client}).InstanceStates(t.Context())

			require.NoError(t, err)
			assert.Equal(t, testCase.expectedCalls, client.calls)
			assert.Equal(t, testCase.expectedStates, states)
		})
	}

	t.Run("wraps the API error", func(t *testing.T) {
		t.Parallel()

		client := &fakeRDSClient{pages: nil, err: errDescribeRefused, calls: 0}

		states, err := (&ResourceIDResolver{svc: client}).InstanceStates(t.Context())

		require.ErrorIs(t, err, errDescribeRefused)
		assert.Empty(t, states)
	})

	t.Run("keeps the states of the pages it read", func(t *testing.T) {
		t.Parallel()

		client := &fakeRDSClient{
			pages: []*rds.DescribeDBInstancesOutput{
				describePage(aws.String("next"), dbInstance("first", firstResourceID, aws.Int32(60))),
			},
			err:   errDescribeRefused,
			calls: 0,
		}

		states, err := (&ResourceIDResolver{svc: client}).InstanceStates(t.Context())

		require.ErrorIs(t, err, errDescribeRefused)
		assert.Equal(t, map[string]InstanceState{
			"first": {ResourceID: firstResourceID, MonitoringInterval: time.Minute},
		}, states, "an instance missing from the result loses its monitoring, so a failed page must not discard the rest")
	})
}
