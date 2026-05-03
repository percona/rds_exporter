package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/percona/rds_exporter/basic"
	rdsclient "github.com/percona/rds_exporter/client"
	"github.com/percona/rds_exporter/config"
	"github.com/percona/rds_exporter/enhanced"
	"github.com/percona/rds_exporter/sessions"
)

//nolint:lll
var (
	listenAddressF       = kingpin.Flag("web.listen-address", "Address on which to expose metrics and web interface.").Default(":9042").String()
	basicMetricsPathF    = kingpin.Flag("web.basic-telemetry-path", "Path under which to expose exporter's basic metrics.").Default("/basic").String()
	enhancedMetricsPathF = kingpin.Flag("web.enhanced-telemetry-path", "Path under which to expose exporter's enhanced metrics.").Default("/enhanced").String()
	configFileF          = kingpin.Flag("config.file", "Path to configuration file.").Default("config.yml").String()
	logTraceF            = kingpin.Flag("log.trace", "Enable verbose tracing of AWS requests (will log credentials).").Default("false").Bool()
	logger               = log.NewNopLogger()
)

func main() {
	kingpin.HelpFlag.Short('h')
	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.Version(version.Print("rds_exporter"))
	kingpin.Parse()
	logger = promlog.New(promlogConfig)
	level.Info(logger).Log("msg", fmt.Sprintf("Starting RDS exporter %s", version.Info()))
	level.Info(logger).Log("msg", fmt.Sprintf("Build context %s", version.BuildContext()))

	cfg, err := config.Load(*configFileF)
	if err != nil {
		level.Error(logger).Log("msg", "Can't read configuration file", "error", err)
		os.Exit(1)
	}

	client := rdsclient.New(logger)
	sess, err := sessions.New(cfg.Instances, client.HTTP(), logger, *logTraceF)
	if err != nil {
		level.Error(logger).Log("msg", "Can't create sessions", "error", err)
		os.Exit(1)
	}

	// basic metrics + client metrics + exporter own metrics (ProcessCollector and GoCollector)
	{
		prometheus.MustRegister(basic.New(cfg, sess, logger))
		prometheus.MustRegister(client)
		http.Handle(*basicMetricsPathF, promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{
			//ErrorLog:      log.NewErrorLogger(), TODO TS
			ErrorHandling: promhttp.ContinueOnError,
		}))
	}

	// enhanced metrics
	{
		registry := prometheus.NewRegistry()
		registry.MustRegister(enhanced.NewCollector(sess, logger))
		http.Handle(*enhancedMetricsPathF, promhttp.HandlerFor(registry, promhttp.HandlerOpts{
			//ErrorLog:      log.NewErrorLogger(), TODO TS
			ErrorHandling: promhttp.ContinueOnError,
		}))
	}

	level.Info(logger).Log("msg", fmt.Sprintf("Basic metrics   : http://%s%s", *listenAddressF, *basicMetricsPathF))
	level.Info(logger).Log("msg", fmt.Sprintf("Enhanced metrics: http://%s%s", *listenAddressF, *enhancedMetricsPathF))

	level.Error(logger).Log("error", http.ListenAndServe(*listenAddressF, nil))
}
