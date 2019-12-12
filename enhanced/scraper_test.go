package enhanced

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/percona/rds_exporter/client"
	"github.com/percona/rds_exporter/config"
	"github.com/percona/rds_exporter/sessions"
)

func TestScraper(t *testing.T) {
	cfg, err := config.Load("../config.tests.yml")
	require.NoError(t, err)
	client := client.New()
	sess, err := sessions.New(cfg.Instances, client.HTTP(), false)
	require.NoError(t, err)

	session, instance := sess.GetSession("us-west-2", "autotest-mysql-57")
	require.NotNil(t, session)
	require.NotNil(t, instance)
	scraper := newScraper(session, []sessions.Instance{*instance})
	scraper.scrape(context.Background())
}

func TestBetterTimes(t *testing.T) {
	type testdata struct {
		allTimes              map[string][]time.Time
		expectedTimes         map[string]time.Time
		expectedNextStartTime time.Time
	}
	for _, td := range []testdata{
		{
			allTimes: map[string][]time.Time{
				"1": {
					time.Date(2018, 9, 29, 16, 25, 42, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 26, 42, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 27, 42, 0, time.UTC),
				},
				"2": {
					time.Date(2018, 9, 29, 16, 25, 46, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 26, 46, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 27, 46, 0, time.UTC),
				},
				"3": {
					time.Date(2018, 9, 29, 16, 25, 51, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 26, 51, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 27, 51, 0, time.UTC),
				},
				"4": {
					time.Date(2018, 9, 29, 16, 26, 3, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 27, 3, 0, time.UTC),
					time.Date(2018, 9, 29, 16, 28, 3, 0, time.UTC),
				},
			},
			expectedTimes: map[string]time.Time{
				"1": time.Date(2018, 9, 29, 16, 27, 42, 0, time.UTC),
				"2": time.Date(2018, 9, 29, 16, 27, 46, 0, time.UTC),
				"3": time.Date(2018, 9, 29, 16, 27, 51, 0, time.UTC),
				"4": time.Date(2018, 9, 29, 16, 28, 3, 0, time.UTC),
			},
			expectedNextStartTime: time.Date(2018, 9, 29, 16, 27, 42, 0, time.UTC),
		},
	} {
		times, nextStartTime := betterTimes(td.allTimes)
		assert.Equal(t, td.expectedTimes, times)
		assert.Equal(t, td.expectedNextStartTime, nextStartTime)
	}
}
