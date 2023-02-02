package sessions

import (
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/rds"
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

// Sessions is a pool of AWS sessions.
type Sessions struct {
	sessions map[*session.Session][]Instance
}

// New creates a new sessions pool for given configuration.
func New(instances []config.Instance, client *http.Client, logger log.Logger, trace bool) (*Sessions, error) {
	logger = log.With(logger, "component", "sessions")
	level.Info(logger).Log("msg", "Creating sessions...")
	res := &Sessions{
		sessions: make(map[*session.Session][]Instance),
	}

	sharedSessions := make(map[string]*session.Session) // region/key => session
	for _, instance := range instances {
		// re-use session for the same region and key (explicit or empty for implicit) pair
		if s := sharedSessions[instance.Region+"/"+instance.AWSAccessKey]; s != nil {
			res.sessions[s] = append(res.sessions[s], Instance{
				Region:                 instance.Region,
				Instance:               instance.Instance,
				Labels:                 instance.Labels,
				DisableBasicMetrics:    instance.DisableBasicMetrics,
				DisableEnhancedMetrics: instance.DisableEnhancedMetrics,
			})
			continue
		}

		// use given credentials, or default credential chain
		var creds *credentials.Credentials

		creds, err := buildCredentials(instance)

		if err != nil {
			return nil, err
		}

		// make config with careful logging
		awsCfg := &aws.Config{
			Credentials: creds,
			Region:      aws.String(instance.Region),
			HTTPClient:  client,
		}
		if trace {
			// fail-safe
			if _, ok := os.LookupEnv("CI"); ok {
				panic("Do not enable AWS request tracing on CI - output will contain credentials.")
			}

			awsCfg.Logger = aws.LoggerFunc(func(args ...interface{}) {
				level.Debug(logger).Log("msg", args)
			})
			awsCfg.CredentialsChainVerboseErrors = aws.Bool(true)
			level := aws.LogDebugWithSigning | aws.LogDebugWithHTTPBody
			level |= aws.LogDebugWithRequestRetries | aws.LogDebugWithRequestErrors | aws.LogDebugWithEventStreamBody
			awsCfg.LogLevel = aws.LogLevel(level)
		}

		// store session
		s, err := session.NewSession(awsCfg)
		if err != nil {
			return nil, err
		}
		sharedSessions[instance.Region+"/"+instance.AWSAccessKey] = s
		res.sessions[s] = append(res.sessions[s], Instance{
			Region:                 instance.Region,
			Instance:               instance.Instance,
			Labels:                 instance.Labels,
			DisableBasicMetrics:    instance.DisableBasicMetrics,
			DisableEnhancedMetrics: instance.DisableEnhancedMetrics,
		})
	}

	// add resource ID to all instances
	for session, instances := range res.sessions {
		svc := rds.New(session)
		var marker *string
		for {
			output, err := svc.DescribeDBInstances(&rds.DescribeDBInstancesInput{
				Marker: marker,
			})
			if err != nil {
				level.Error(logger).Log("msg", "Failed to get resource IDs.", "error", err)
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
	for session, instances := range res.sessions {
		newInstances := make([]Instance, 0, len(instances))
		for _, instance := range instances {
			if instance.ResourceID == "" {
				level.Error(logger).Log("msg", fmt.Sprintf("Skipping %s - can't determine resourceID.", instance))
				continue
			}
			newInstances = append(newInstances, instance)
		}
		res.sessions[session] = newInstances
	}

	// remove sessions without instances
	for _, s := range sharedSessions {
		if len(res.sessions[s]) == 0 {
			delete(res.sessions, s)
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

	level.Info(logger).Log("msg", fmt.Sprintf("Using %d sessions.", len(res.sessions)))
	return res, nil
}

// GetSession returns session and full instance information for given region and instance.
func (s *Sessions) GetSession(region, instance string) (*session.Session, *Instance) {
	for session, instances := range s.sessions {
		for _, i := range instances {
			if i.Region == region && i.Instance == instance {
				return session, &i
			}
		}
	}
	return nil, nil
}

func buildCredentials(instance config.Instance) (*credentials.Credentials, error) {
	if instance.AWSRoleArn != "" {
		stsSession, err := session.NewSession(&aws.Config{
			Region:      aws.String(instance.Region),
			Credentials: credentials.NewStaticCredentials(instance.AWSAccessKey, instance.AWSSecretKey, ""),
		})
		if err != nil {
			return nil, err
		}

		return stscreds.NewCredentials(stsSession, instance.AWSRoleArn), nil
	}
	if instance.AWSAccessKey != "" || instance.AWSSecretKey != "" {
		return credentials.NewCredentials(&credentials.StaticProvider{
			Value: credentials.Value{
				AccessKeyID:     instance.AWSAccessKey,
				SecretAccessKey: instance.AWSSecretKey,
			},
		}), nil
	}
	return nil, nil
}

// AllSessions returns all sessions and instances.
func (s *Sessions) AllSessions() map[*session.Session][]Instance {
	return s.sessions
}
