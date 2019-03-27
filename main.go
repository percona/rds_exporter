package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/hellofresh/rds_exporter/basic"
	"github.com/hellofresh/rds_exporter/client"
	"github.com/hellofresh/rds_exporter/config"
	"github.com/hellofresh/rds_exporter/enhanced"
	"github.com/hellofresh/rds_exporter/sessions"
)

//nolint:lll
var (
	listenAddressF       = kingpin.Flag("web.listen-address", "Address on which to expose metrics and web interface.").Default(":9042").String()
	basicMetricsPathF    = kingpin.Flag("web.basic-telemetry-path", "Path under which to expose exporter's basic metrics.").Default("/basic").String()
	enhancedMetricsPathF = kingpin.Flag("web.enhanced-telemetry-path", "Path under which to expose exporter's enhanced metrics.").Default("/enhanced").String()
	configFileF          = kingpin.Flag("config.file", "Path to configuration file.").Default("config.yml").String()
	logTraceF            = kingpin.Flag("log.trace", "Enable verbose tracing of AWS requests (will log credentials).").Default("false").Bool()
)

func main() {
	log.AddFlags(kingpin.CommandLine)
	log.Infoln("Starting RDS exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())
	kingpin.Parse()

	cfg, err := config.Load(*configFileF)
	if err != nil {
		log.Fatalf("Can't read configuration file: %s", err)
	}

	client := client.New()
	sess, err := sessions.New(cfg.Instances, client.HTTP(), *logTraceF)
	if err != nil {
		log.Fatalf("Can't create sessions: %s", err)
	}

	// basic metrics + client metrics + exporter own metrics (ProcessCollector and GoCollector)
	{
		prometheus.MustRegister(basic.New(cfg, sess))
		prometheus.MustRegister(client)
		http.Handle(*basicMetricsPathF, promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{
			ErrorLog:      log.NewErrorLogger(),
			ErrorHandling: promhttp.ContinueOnError,
		}))
	}

	// enhanced metrics
	{
		registry := prometheus.NewRegistry()
		registry.MustRegister(enhanced.NewCollector(sess))
		http.Handle(*enhancedMetricsPathF, promhttp.HandlerFor(registry, promhttp.HandlerOpts{
			ErrorLog:      log.NewErrorLogger(),
			ErrorHandling: promhttp.ContinueOnError,
		}))
	}

	log.Infof("Basic metrics   : http://%s%s", *listenAddressF, *basicMetricsPathF)
	log.Infof("Enhanced metrics: http://%s%s", *listenAddressF, *enhancedMetricsPathF)
	log.Fatal(http.ListenAndServe(*listenAddressF, nil))
}
