package main

import (
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/percona/rds_exporter/basic"
	"github.com/percona/rds_exporter/config"
	"github.com/percona/rds_exporter/enhanced"
	"github.com/percona/rds_exporter/sessions"
)

var (
	listenAddress       = kingpin.Flag("web.listen-address", "Address on which to expose metrics and web interface.").Default(":9042").String()
	basicMetricsPath    = kingpin.Flag("web.basic-telemetry-path", "Path under which to expose exporter's basic metrics.").Default("/basic").String()
	enhancedMetricsPath = kingpin.Flag("web.enhanced-telemetry-path", "Path under which to expose exporter's enhanced metrics.").Default("/enhanced").String()
	configFile          = kingpin.Flag("config.file", "Path to configuration file.").Default("config.yml").String()

	settings     *config.Settings
	sessionsPool *sessions.Sessions
)

// handleReload handles a full reload of the configuration file and regenerates the collector templates.
func handleReload(w http.ResponseWriter, req *http.Request) {
	err := settings.Load(*configFile)
	if err != nil {
		str := fmt.Sprintf("Can't read configuration file: %s", err.Error())
		fmt.Fprintln(w, str)
		log.Errorln(str)
	}
	fmt.Fprintln(w, "Reload complete")
}

func main() {
	log.AddFlags(kingpin.CommandLine)
	log.Infoln("Starting RDS exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())

	kingpin.Parse()

	// Create settings.
	settings = &config.Settings{}

	// Create sessions pool.
	sessionsPool = &sessions.Sessions{}

	// Reset sessions pool after every settings reload.
	settings.AfterLoad = func(config config.Config) error {
		return sessionsPool.Load(config.Instances)
	}

	// Read configuration from file.
	err := settings.Load(*configFile)
	if err != nil {
		log.Fatalf("Can't read configuration file: %s\n", err.Error())
	}

	// Basic Metrics
	{
		// Create new Exporter with provided settings and session pool.
		exporter := basic.New(settings, sessionsPool)
		registry := prometheus.NewRegistry()
		registry.MustRegister(exporter)
		handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
		// Expose the exporter's own metrics on given path
		http.Handle(*basicMetricsPath, handler)
	}

	// Enhanced Metrics
	{
		// Create new Exporter with provided settings and session pool.
		exporter := enhanced.New(settings, sessionsPool)
		registry := prometheus.NewRegistry()
		registry.MustRegister(exporter)
		handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
		// Expose the exporter's own metrics on given path
		http.Handle(*enhancedMetricsPath, handler)
	}

	// Allows manual reload of the configuration
	http.HandleFunc("/reload", handleReload)

	// Inform user we are ready.
	log.Infoln("RDS exporter started")
	log.Infoln("Listening on", *listenAddress)

	// Start serving for clients
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}
