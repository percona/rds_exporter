package main

import (
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/coinsph/rds_exporter/basic"
	"github.com/coinsph/rds_exporter/client"
	"github.com/coinsph/rds_exporter/config"
	"github.com/coinsph/rds_exporter/enhanced"
	"github.com/coinsph/rds_exporter/sessions"
)

//nolint:lll
var (
	listenAddressF = kingpin.Flag("web.listen-address", "Address on which to expose metrics and web interface.").Default(":9042").String()
	metricsPathF   = kingpin.Flag("web.telemetry-path", "Path under which to expose exporter's metrics.").Default("/metrics").String()
	configFileF    = kingpin.Flag("config.file", "Path to configuration file.").Default("config.yml").String()
	logTraceF      = kingpin.Flag("log.trace", "Enable verbose tracing of AWS requests (will log credentials).").Default("false").Bool()
	checkF         = kingpin.Flag("check", "Binary check").Default("false").Bool()
)

func main() {
	log.AddFlags(kingpin.CommandLine)
	kingpin.Parse()
	if *checkF {
		os.Exit(0)
	}
	log.Infoln("Starting RDS exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())

	cfg, err := config.Load(*configFileF)
	if err != nil {
		log.Fatalf("Can't read configuration file: %s", err)
	}

	if len(cfg.BasicInstances) == 0 {
		log.Fatalf("Basic instances must be present\n")
	}

	client := client.New()
	// basic metrics + client metrics + exporter own metrics (ProcessCollector and GoCollector)
	sessBasic, err := sessions.New(cfg.BasicInstances, client.HTTP(), *logTraceF)
	if err != nil {
		log.Fatalf("Can't create basic sessions: %s", err)
	}
	{
		prometheus.MustRegister(basic.New(cfg, sessBasic))
		prometheus.MustRegister(client)
		http.Handle(*metricsPathF, promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{
			ErrorLog:      log.NewErrorLogger(),
			ErrorHandling: promhttp.ContinueOnError,
		}))
	}
	log.Infof("Start listen metrics: http://%s%s", *listenAddressF, *metricsPathF)

	// enhanced metrics
	if len(cfg.EnhancedInstances) > 0 {
		sessEnhanced, err := sessions.New(cfg.EnhancedInstances, client.HTTP(), *logTraceF)
		if err != nil {
			log.Fatalf("Can't create enhanced sessions: %s", err)
		}
		{
			prometheus.MustRegister(enhanced.NewCollector(sessEnhanced))
		}
		log.Infof("Enhanced metrics was enabled\n")
	}

	log.Fatal(http.ListenAndServe(*listenAddressF, nil))
}
