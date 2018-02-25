// The following directive is necessary to make the package coherent:
// This program generates metrics.go. It can be invoked by running
// go generate
package main

import (
	"log"
	"os"
	"sort"
	"text/template"
)

type Metric string

func (m Metric) FqName() string {
	switch m {
	case "FreeStorageSpace":
		return "node_filesystem_free"
	case "FreeableMemory":
		return "node_memory_Cached"
	case "CPUUtilization":
		return "node_cpu_average"
	case "EngineUptime":
		return "node_boot_time"
	}

	return safeName("AWS/RDS_" + toSnakeCase(string(m)) + "_average")
}

func (m Metric) Labels() []string {
	return []string{
		"instance",
		"region",
	}
}

func (m Metric) ConstLabels() map[string]string {
	switch m {
	case "CPUUtilization":
		return map[string]string{
			"cpu":  "All",
			"mode": "idle",
		}
	}

	return nil
}

func (m Metric) Name() string {
	return string(m)
}

func (m Metric) Help() string {
	if v, ok := doc[m.Name()]; ok {
		return v
	}

	return m.Name()
}

var (
	metrics = []Metric{
		"ActiveTransactions",
		"AuroraBinlogReplicaLag",
		"AuroraReplicaLag",
		"AuroraReplicaLagMaximum",
		"AuroraReplicaLagMinimum",
		"BinLogDiskUsage",
		"BlockedTransactions",
		"BufferCacheHitRatio",
		"BurstBalance",
		"CommitLatency",
		"CommitThroughput",
		"CPUCreditBalance",
		"CPUCreditUsage",
		"CPUUtilization",
		"DatabaseConnections",
		"DDLLatency",
		"DDLThroughput",
		"Deadlocks",
		"DeleteLatency",
		"DeleteThroughput",
		"DiskQueueDepth",
		"DMLLatency",
		"DMLThroughput",
		"EngineUptime",
		"FreeableMemory",
		"FreeLocalStorage",
		"FreeStorageSpace",
		"InsertLatency",
		"InsertThroughput",
		"LoginFailures",
		"NetworkReceiveThroughput",
		"NetworkThroughput",
		"NetworkTransmitThroughput",
		"Queries",
		"ReadIOPS",
		"ReadLatency",
		"ReadThroughput",
		"ResultSetCacheHitRatio",
		"SelectLatency",
		"SelectThroughput",
		"SwapUsage",
		"UpdateLatency",
		"UpdateThroughput",
		"VolumeBytesUsed",
		"VolumeReadIOPs",
		"VolumeWriteIOPs",
		"WriteIOPS",
		"WriteLatency",
		"WriteThroughput",
	}

	doc = map[string]string{
		"BinLogDiskUsage":           "The amount of disk space occupied by binary logs on the master. Applies to MySQL read replicas. Units: Bytes",
		"BurstBalance":              "The percent of General Purpose SSD (gp2) burst-bucket I/O credits available. Units: Percent",
		"CPUUtilization":            "The percentage of CPU utilization. Units: Percent",
		"CPUCreditUsage":            "[T2 instances] The number of CPU credits consumed by the instance. One CPU credit equals one vCPU running at 100% utilization for one minute or an equivalent combination of vCPUs, utilization, and time (for example, one vCPU running at 50% utilization for two minutes or two vCPUs running at 25% utilization for two minutes). CPU credit metrics are available only at a 5 minute frequency. If you specify a period greater than five minutes, use the Sum statistic instead of the Average statistic. Units: Count",
		"CPUCreditBalance":          "[T2 instances] The number of CPU credits available for the instance to burst beyond its base CPU utilization. Credits are stored in the credit balance after they are earned and removed from the credit balance after they expire. Credits expire 24 hours after they are earned. CPU credit metrics are available only at a 5 minute frequency. Units: Count",
		"DatabaseConnections":       "The number of database connections in use. Units: Count",
		"DiskQueueDepth":            "The number of outstanding IOs (read/write requests) waiting to access the disk. Units: Count",
		"FreeableMemory":            "The amount of available random access memory. Units: Bytes",
		"FreeStorageSpace":          "The amount of available storage space. Units: Bytes",
		"MaximumUsedTransactionIDs": "The maximum transaction ID that has been used. Applies to PostgreSQL. Units: Count",
		"NetworkReceiveThroughput":  "The incoming (Receive) network traffic on the DB instance, including both customer database traffic and Amazon RDS traffic used for monitoring and replication. Units: Bytes/second",
		"NetworkTransmitThroughput": "The outgoing (Transmit) network traffic on the DB instance, including both customer database traffic and Amazon RDS traffic used for monitoring and replication. Units: Bytes/second",
		"OldestReplicationSlotLag":  "The lagging size of the replica lagging the most in terms of WAL data received. Applies to PostgreSQL. Units: Megabytes",
		"ReadIOPS":                  "The average number of disk I/O operations per second. Units: Count/Second",
		"ReadLatency":               "The average amount of time taken per disk I/O operation. Units: Seconds",
		"ReadThroughput":            "The average number of bytes read from disk per second. Units: Bytes/Second",
		"ReplicaLag":                "The amount of time a Read Replica DB instance lags behind the source DB instance. Applies to MySQL, MariaDB, and PostgreSQL Read Replicas. Units: Seconds",
		"ReplicationSlotDiskUsage":  "The disk space used by replication slot files. Applies to PostgreSQL. Units: Megabytes",
		"SwapUsage":                 "The amount of swap space used on the DB instance. Units: Bytes",
		"TransactionLogsDiskUsage":  "The disk space used by transaction logs. Applies to PostgreSQL. Units: Megabytes",
		"TransactionLogsGeneration": "The size of transaction logs generated per second. Applies to PostgreSQL. Units: Megabytes/second",
		"WriteIOPS":                 "The average number of disk I/O operations per second. Units: Count/Second",
		"WriteLatency":              "The average amount of time taken per disk I/O operation. Units: Seconds",
		"WriteThroughput":           "The average number of bytes written to disk per second. Units: Bytes/Second",
	}
)

func main() {
	f, err := os.Create("metrics.go")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	sort.SliceStable(metrics, func(i, j int) bool {
		return metrics[i] < metrics[j]
	})
	packageTemplate.Execute(f, struct {
		Metrics []Metric
	}{
		Metrics: metrics,
	})
}

var packageTemplate = template.Must(template.New("").Parse(`// Code generated by go generate; DO NOT EDIT.
package basic

import (
	"github.com/prometheus/client_golang/prometheus"
)

var Metrics = []Metric{
{{- range .Metrics }}
	{
		Name: "{{.}}",
		Desc: prometheus.NewDesc(
			"{{.FqName}}",
			"{{.Help}}",
			{{printf "%#v" .Labels}},
			{{printf "%#v" .ConstLabels}},
		),
	},
{{- end }}
}
`))
