package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/Percona-Lab/rds_exporter/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	listenAddress = flag.String("web.listen-address", ":9042", "Address on which to expose metrics and web interface.")
	metricsPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose exporter's metrics.")
	scrapePath    = flag.String("web.telemetry-scrape-path", "/scrape", "Path under which to expose CloudWatch metrics.")
	configFile    = flag.String("config.file", "config.yml", "Path to configuration file.")

	globalRegistry *prometheus.Registry
	settings       *config.Settings
	totalRequests  prometheus.Counter
	configMutex    = &sync.Mutex{}
)

func loadConfigFile() error {
	var err error
	var tmpSettings *config.Settings
	configMutex.Lock()

	// Initial loading of the configuration file
	tmpSettings, err = config.Load(*configFile)
	if err != nil {
		return err
	}

	generateTemplates(tmpSettings)

	settings = tmpSettings
	configMutex.Unlock()

	return nil
}

// handleReload handles a full reload of the configuration file and regenerates the collector templates.
func handleReload(w http.ResponseWriter, req *http.Request) {
	err := loadConfigFile()
	if err != nil {
		str := fmt.Sprintf("Can't read configuration file: %s", err.Error())
		fmt.Fprintln(w, str)
		fmt.Println(str)
	}
	fmt.Fprintln(w, "Reload complete")
}

// handleTarget handles scrape requests which make use of CloudWatch service
func handleTarget(w http.ResponseWriter, req *http.Request) {
	urlQuery := req.URL.Query()

	target := urlQuery.Get("target")
	task := urlQuery.Get("task")
	region := urlQuery.Get("region")

	// Check if we have all the required parameters in the URL
	if task == "" {
		fmt.Fprintln(w, "Error: Missing task parameter")
		return
	}

	configMutex.Lock()
	registry := prometheus.NewRegistry()
	collector, err := NewCwCollector(target, task, region)
	if err != nil {
		// Can't create the collector, display error
		fmt.Fprintf(w, "Error: %s\n", err.Error())
		configMutex.Unlock()
		return
	}

	registry.MustRegister(collector)
	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		DisableCompression: false,
	})

	// Serve the answer through the Collect method of the Collector
	handler.ServeHTTP(w, req)
	configMutex.Unlock()
}

func main() {
	flag.Parse()

	globalRegistry = prometheus.NewRegistry()

	totalRequests = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cloudwatch_requests_total",
		Help: "API requests made to CloudWatch",
	})

	globalRegistry.MustRegister(totalRequests)

	prometheus.DefaultGatherer = globalRegistry

	err := loadConfigFile()
	if err != nil {
		fmt.Printf("Can't read configuration file: %s\n", err.Error())
		os.Exit(-1)
	}

	fmt.Println("CloudWatch exporter started...")

	// Expose the exporter's own metrics on /metrics
	http.Handle(*metricsPath, promhttp.Handler())

	// Expose CloudWatch through this endpoint
	http.HandleFunc(*scrapePath, handleTarget)

	// Allows manual reload of the configuration
	http.HandleFunc("/reload", handleReload)

	// Start serving for clients
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}
