package sessions

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	"github.com/percona/rds_exporter/config"
)

// Instance represents a single RDS instance information in runtime.
type Instance struct {
	Region                     string
	Instance                   string
	DisableBasicMetrics        bool
	DisableEnhancedMetrics     bool
	ResourceID                 string
	Labels                     map[string]string
	EnhancedMonitoringInterval time.Duration
}

func (i Instance) String() string {
	res := i.Region + "/" + i.Instance
	if i.ResourceID != "" {
		res += " (" + i.ResourceID + ")"
	}
	return res
}

// Sessions is a pool of AWS configs per region.
type Sessions struct {
	sessions map[string][]Instance
	Configs  map[string]aws.Config
}

// InstanceState is the AWS-side state of a single DB instance.
type InstanceState struct {
	ResourceID string
	// MonitoringInterval is zero when Enhanced Monitoring is disabled, in which case AWS publishes
	// no RDSOSMetrics log stream for the instance at all.
	MonitoringInterval time.Duration
}

// ResourceIDResolver resolves the current AWS state of DB instances.
type ResourceIDResolver struct {
	svc rds.DescribeDBInstancesAPIClient
}

// NewResourceIDResolver creates a resolver using the given AWS config.
func NewResourceIDResolver(cfg aws.Config) *ResourceIDResolver {
	return &ResourceIDResolver{
		svc: rds.NewFromConfig(cfg),
	}
}

// InstanceStates returns the current AWS state of every DB instance, keyed by DB instance identifier.
func (r *ResourceIDResolver) InstanceStates(ctx context.Context) (map[string]InstanceState, error) {
	states := make(map[string]InstanceState)

	paginator := rds.NewDescribeDBInstancesPaginator(r.svc, &rds.DescribeDBInstancesInput{}) //nolint:exhaustruct
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to describe DB instances: %w", err)
		}

		for _, dbInstance := range output.DBInstances {
			instanceID := aws.ToString(dbInstance.DBInstanceIdentifier)
			resourceID := aws.ToString(dbInstance.DbiResourceId)
			if instanceID == "" || resourceID == "" {
				continue
			}

			states[instanceID] = InstanceState{
				ResourceID:         resourceID,
				MonitoringInterval: time.Duration(aws.ToInt32(dbInstance.MonitoringInterval)) * time.Second,
			}
		}
	}

	return states, nil
}

// New creates a new sessions pool for given configuration.
func New(instances []config.Instance, client *http.Client, logger log.Logger, trace bool) (*Sessions, error) {
	logger = log.With(logger, "component", "sessions")
	level.Info(logger).Log("msg", "Creating sessions...")

	res := &Sessions{
		sessions: make(map[string][]Instance),
		Configs:  make(map[string]aws.Config),
	}

	for _, instance := range instances {
		key := instance.Region + "/" + instance.AWSAccessKey
		if _, exists := res.Configs[key]; exists {
			res.sessions[key] = append(res.sessions[key], Instance{
				Region:                 instance.Region,
				Instance:               instance.Instance,
				Labels:                 instance.Labels,
				DisableBasicMetrics:    instance.DisableBasicMetrics,
				DisableEnhancedMetrics: instance.DisableEnhancedMetrics,
			})
			continue
		}

		cfg, err := loadAWSConfig(instance, client, trace, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config: %w", err)
		}

		res.Configs[key] = cfg
		res.sessions[key] = append(res.sessions[key], Instance{
			Region:                 instance.Region,
			Instance:               instance.Instance,
			Labels:                 instance.Labels,
			DisableBasicMetrics:    instance.DisableBasicMetrics,
			DisableEnhancedMetrics: instance.DisableEnhancedMetrics,
		})
	}

	// add resource ID and monitoring interval to all instances
	for key, cfg := range res.Configs {
		states, err := NewResourceIDResolver(cfg).InstanceStates(context.Background())
		if err != nil {
			level.Error(logger).Log("msg", "Failed to get instance states.", "error", err)

			continue
		}

		for idx, instance := range res.sessions[key] {
			state, ok := states[instance.Instance]
			if !ok {
				continue
			}

			res.sessions[key][idx].ResourceID = state.ResourceID
			res.sessions[key][idx].EnhancedMonitoringInterval = state.MonitoringInterval
		}
	}

	// remove instances without resource ID
	for key, instances := range res.sessions {
		newInstances := make([]Instance, 0, len(instances))
		for _, instance := range instances {
			if instance.ResourceID == "" {
				level.Error(logger).Log("msg", fmt.Sprintf("Skipping %s - can't determine resourceID.", instance))
				continue
			}
			newInstances = append(newInstances, instance)
		}
		res.sessions[key] = newInstances
	}

	// remove configs without instances
	for key := range res.Configs {
		if len(res.sessions[key]) == 0 {
			delete(res.Configs, key)
			delete(res.sessions, key)
		}
	}

	w := tabwriter.NewWriter(os.Stderr, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Region\tInstance\tResource ID\tInterval\n")
	for _, instances := range res.sessions {
		for _, instance := range instances {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", instance.Region, instance.Instance, instance.ResourceID, instance.EnhancedMonitoringInterval)
		}
	}
	_ = w.Flush()

	level.Info(logger).Log("msg", fmt.Sprintf("Using %d session configs.", len(res.Configs)))
	return res, nil
}

// GetConfig returns AWS config and full instance information for given region and instance.
func (s *Sessions) GetConfig(region, instance string) (*aws.Config, *Instance) {
	for key, instances := range s.sessions {
		for _, i := range instances {
			if i.Region == region && i.Instance == instance {
				cfg := s.Configs[key]
				return &cfg, &i
			}
		}
	}
	return nil, nil
}

// AllSessions returns all AWS configs and instances.
func (s *Sessions) AllSessions() map[string][]Instance {
	return s.sessions
}

// Internal helper
func loadAWSConfig(instance config.Instance, client *http.Client, trace bool, logger log.Logger) (aws.Config, error) {
	options := []func(*awsConfig.LoadOptions) error{
		awsConfig.WithRegion(instance.Region),
		awsConfig.WithHTTPClient(client),
	}

	if instance.IRSAEnabled {
		return awsConfig.LoadDefaultConfig(context.Background(), options...)
	}

	if instance.AWSRoleArn != "" {
		stsCfg, err := awsConfig.LoadDefaultConfig(context.Background(), options...)
		if err != nil {
			return aws.Config{}, err
		}

		stsClient := sts.NewFromConfig(stsCfg)
		provider := stscreds.NewAssumeRoleProvider(stsClient, instance.AWSRoleArn)
		stsCfg.Credentials = aws.NewCredentialsCache(provider)
		return stsCfg, nil
	}

	if instance.AWSAccessKey != "" && instance.AWSSecretKey != "" {
		staticCreds := aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
			instance.AWSAccessKey,
			instance.AWSSecretKey,
			"",
		))
		options = append(options, awsConfig.WithCredentialsProvider(staticCreds))
		return awsConfig.LoadDefaultConfig(context.Background(), options...)
	}

	return awsConfig.LoadDefaultConfig(context.Background(), options...)
}
