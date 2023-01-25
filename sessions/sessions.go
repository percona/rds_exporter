//https://aws.github.io/aws-sdk-go-v2/docs/migrating/

package sessions

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/prometheus/common/log"

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

// Configs is a pool of AWS configs.
type Configs struct {
	configs map[*aws.Config][]Instance
}

// New creates a new sessions pool for given configuration.
func New(instances []config.Instance, client *http.Client, trace bool) (*Configs, error) {
	logger := log.With("component", "sessions")
	logger.Info("Creating sessions...")
	res := &Configs{
		configs: make(map[*aws.Config][]Instance),
	}

	configs := make(map[string]*aws.Config) // region/key => session
	for _, instance := range instances {
		// re-use session for the same region and key (explicit or empty for implicit) pair
		if s := configs[instance.Region+"/"+instance.AWSAccessKey]; s != nil {
			res.configs[s] = append(res.configs[s], Instance{
				Region:                 instance.Region,
				Instance:               instance.Instance,
				Labels:                 instance.Labels,
				DisableBasicMetrics:    instance.DisableBasicMetrics,
				DisableEnhancedMetrics: instance.DisableEnhancedMetrics,
			})
			continue
		}

		awsCfg, err := buildConfig(instance, client, trace)

		if err != nil {
			return nil, err
		}

		configs[instance.Region+"/"+instance.AWSAccessKey] = awsCfg
		res.configs[awsCfg] = append(res.configs[awsCfg], Instance{
			Region:                 instance.Region,
			Instance:               instance.Instance,
			Labels:                 instance.Labels,
			DisableBasicMetrics:    instance.DisableBasicMetrics,
			DisableEnhancedMetrics: instance.DisableEnhancedMetrics,
		})
	}

	// add resource ID to all instances
	for config, instances := range res.configs {
		svc := rds.NewFromConfig(*config)
		var marker *string
		for {
			output, err := svc.DescribeDBInstances(context.TODO(), &rds.DescribeDBInstancesInput{
				Marker: marker,
			})
			if err != nil {
				logger.Errorf("Failed to get resource IDs: %s.", err)
				break
			}

			for _, dbInstance := range output.DBInstances {
				for i, instance := range instances {
					if *dbInstance.DBInstanceIdentifier == instance.Instance {
						instances[i].ResourceID = *dbInstance.DbiResourceId
						instances[i].EnhancedMonitoringInterval = time.Duration(*dbInstance.MonitoringInterval) * time.Second
					}
				}
			}
			if marker = output.Marker; marker == nil {
				break
			}
		}
	}

	// remove instances without resource ID
	for session, instances := range res.configs {
		newInstances := make([]Instance, 0, len(instances))
		for _, instance := range instances {
			if instance.ResourceID == "" {
				logger.Errorf("Skipping %s - can't determine resourceID.", instance)
				continue
			}
			newInstances = append(newInstances, instance)
		}
		res.configs[session] = newInstances
	}

	// remove sessions without instances
	for _, s := range configs {
		if len(res.configs[s]) == 0 {
			delete(res.configs, s)
		}
	}

	w := tabwriter.NewWriter(os.Stderr, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Region\tInstance\tResource ID\tInterval\n")
	for _, instances := range res.configs {
		for _, instance := range instances {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", instance.Region, instance.Instance, instance.ResourceID, instance.EnhancedMonitoringInterval)
		}
	}
	_ = w.Flush()

	logger.Infof("Using %d sessions.", len(res.configs))
	return res, nil
}

// GetSession returns session and full instance information for given region and instance.
func (s *Configs) GetSession(region, instance string) (*aws.Config, *Instance) {
	for config, instances := range s.configs {
		for _, i := range instances {
			if i.Region == region && i.Instance == instance {
				return config, &i
			}
		}
	}
	return nil, nil
}

func buildConfig(instance config.Instance, httpClient *http.Client, trace bool) (*aws.Config, error) {
	ctx := context.TODO()

	options := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(instance.Region), awsconfig.WithHTTPClient(httpClient),
	}

	if trace {
		// fail-safe
		if _, ok := os.LookupEnv("CI"); ok {
			panic("Do not enable AWS request tracing on CI - output will contain credentials.")
		}
		level := aws.LogSigning | aws.LogRequestWithBody
		level |= aws.LogRetries | aws.LogRequest | aws.LogRequestWithBody | aws.LogResponseWithBody
		options = append(options, awsconfig.WithClientLogMode(level))
	}

	if instance.AWSAccessKey != "" && instance.AWSSecretKey != "" {
		creds := aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(instance.AWSAccessKey, instance.AWSSecretKey, ""))
		options = append(options, awsconfig.WithCredentialsProvider(creds))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, options...)

	if err != nil {
		return nil, err
	}

	if instance.AWSRoleArn != "" {
		client := sts.NewFromConfig(cfg)
		creds := stscreds.NewAssumeRoleProvider(client, instance.AWSRoleArn)
		return &aws.Config{
			Credentials:   creds,
			Region:        instance.Region,
			HTTPClient:    httpClient,
			ClientLogMode: cfg.ClientLogMode,
		}, nil
	}

	return &cfg, nil
}

// AllConfigs returns all aws configs and instances.
func (s *Configs) AllConfigs() map[*aws.Config][]Instance {
	return s.configs
}
