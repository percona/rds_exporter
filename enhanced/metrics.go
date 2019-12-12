package enhanced

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// osMetrics represents available Enhanced Monitoring OS metrics from CloudWatch Logs.
//
// See https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_Monitoring.OS.html#USER_Monitoring.OS.CloudWatchLogs
//
// Confusingly, some of the present metrics (for example, "writeLatency") are documented at
// https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/rds-metricscollected.html
//
//nolint:lll
type osMetrics struct {
	Engine             string    `json:"engine"             help:"The database engine for the DB instance."`
	InstanceID         string    `json:"instanceID"         help:"The DB instance identifier."`
	InstanceResourceID string    `json:"instanceResourceID" help:"A region-unique, immutable identifier for the DB instance, also used as the log stream identifier."`
	NumVCPUs           int       `json:"numVCPUs"           help:"The number of virtual CPUs for the DB instance."`
	Timestamp          time.Time `json:"timestamp"          help:"The time at which the metrics were taken."`
	Uptime             string    `json:"uptime"             help:"The amount of time that the DB instance has been active."`
	Version            float64   `json:"version"            help:"The version of the OS metrics' stream JSON format."`

	CPUUtilization    cpuUtilization    `json:"cpuUtilization"`
	DiskIO            []diskIO          `json:"diskIO"`
	FileSys           []fileSys         `json:"fileSys"`
	LoadAverageMinute loadAverageMinute `json:"loadAverageMinute"`
	Memory            memory            `json:"memory"`
	Network           []network         `json:"network"`
	ProcessList       []processList     `json:"processList"`
	Swap              swap              `json:"swap"`
	Tasks             tasks             `json:"tasks"`

	// TODO Handle this: https://jira.percona.com/browse/PMM-3835
	PhysicalDeviceIO []diskIO `json:"physicalDeviceIO"`
}

type cpuUtilization struct {
	Guest  float64 `json:"guest"  help:"The percentage of CPU in use by guest programs."`
	Idle   float64 `json:"idle"   help:"The percentage of CPU that is idle."`
	Irq    float64 `json:"irq"    help:"The percentage of CPU in use by software interrupts."`
	Nice   float64 `json:"nice"   help:"The percentage of CPU in use by programs running at lowest priority."`
	Steal  float64 `json:"steal"  help:"The percentage of CPU in use by other virtual machines."`
	System float64 `json:"system" help:"The percentage of CPU in use by the kernel."`
	Total  float64 `json:"total"  help:"The total percentage of the CPU in use. This value includes the nice value."`
	User   float64 `json:"user"   help:"The percentage of CPU in use by user programs."`
	Wait   float64 `json:"wait"   help:"The percentage of CPU unused while waiting for I/O access."`
}

//nolint:lll
type diskIO struct {
	// common
	ReadIOsPS  float64 `json:"readIOsPS"  help:"The number of read operations per second."`
	WriteIOsPS float64 `json:"writeIOsPS" help:"The number of write operations per second."`
	Device     string  `json:"device"     help:"The identifier of the disk device in use."`

	// non-Aurora
	AvgQueueLen *float64 `json:"avgQueueLen" help:"The number of requests waiting in the I/O device's queue."`
	AvgReqSz    *float64 `json:"avgReqSz"    help:"The average request size, in kilobytes."`
	Await       *float64 `json:"await"       help:"The number of milliseconds required to respond to requests, including queue time and service time."`
	ReadKb      *int     `json:"readKb"      help:"The total number of kilobytes read."`
	ReadKbPS    *float64 `json:"readKbPS"    help:"The number of kilobytes read per second."`
	RrqmPS      *float64 `json:"rrqmPS"      help:"The number of merged read requests queued per second."`
	TPS         *float64 `json:"tps"         help:"The number of I/O transactions per second."`
	Util        *float64 `json:"util"        help:"The percentage of CPU time during which requests were issued."`
	WriteKb     *int     `json:"writeKb"     help:"The total number of kilobytes written."`
	WriteKbPS   *float64 `json:"writeKbPS"   help:"The number of kilobytes written per second."`
	WrqmPS      *float64 `json:"wrqmPS"      help:"The number of merged write requests queued per second."`

	// Aurora
	DiskQueueDepth  *float64 `json:"diskQueueDepth"  help:"The number of outstanding IOs (read/write requests) waiting to access the disk."`
	ReadLatency     *float64 `json:"readLatency"     help:"The average amount of time taken per disk I/O operation."`
	ReadThroughput  *float64 `json:"readThroughput"  help:"The average number of bytes read from disk per second."`
	WriteLatency    *float64 `json:"writeLatency"    help:"The average amount of time taken per disk I/O operation."`
	WriteThroughput *float64 `json:"writeThroughput" help:"The average number of bytes written to disk per second."`
}

//nolint:lll
type fileSys struct {
	MaxFiles        int     `json:"maxFiles"        help:"The maximum number of files that can be created for the file system."`
	MountPoint      string  `json:"mountPoint"      help:"The path to the file system."`
	Name            string  `json:"name"            help:"The name of the file system."`
	Total           int     `json:"total"           help:"The total number of disk space available for the file system, in kilobytes."`
	Used            int     `json:"used"            help:"The amount of disk space used by files in the file system, in kilobytes."`
	UsedFilePercent float64 `json:"usedFilePercent" help:"The percentage of available files in use."`
	UsedFiles       int     `json:"usedFiles"       help:"The number of files in the file system."`
	UsedPercent     float64 `json:"usedPercent"     help:"The percentage of the file-system disk space in use."`
}

type loadAverageMinute struct {
	Fifteen float64 `json:"fifteen" help:"The number of processes requesting CPU time over the last 15 minutes."`
	Five    float64 `json:"five"    help:"The number of processes requesting CPU time over the last 5 minutes."`
	One     float64 `json:"one"     help:"The number of processes requesting CPU time over the last minute."`
}

//nolint:lll
type memory struct {
	Active         int `json:"active"         node:"Active_bytes"       m:"1024" help:"The amount of assigned memory, in kilobytes."`
	Buffers        int `json:"buffers"        node:"Buffers_bytes"      m:"1024" help:"The amount of memory used for buffering I/O requests prior to writing to the storage device, in kilobytes."`
	Cached         int `json:"cached"         node:"Cached_bytes"       m:"1024" help:"The amount of memory used for caching file system–based I/O."`
	Dirty          int `json:"dirty"          node:"Dirty_bytes"        m:"1024" help:"The amount of memory pages in RAM that have been modified but not written to their related data block in storage, in kilobytes."`
	Free           int `json:"free"           node:"MemFree_bytes"      m:"1024" help:"The amount of unassigned memory, in kilobytes."`
	HugePagesFree  int `json:"hugePagesFree"  node:"HugePages_Free"     m:"1"    help:"The number of free huge pages. Huge pages are a feature of the Linux kernel."`
	HugePagesRsvd  int `json:"hugePagesRsvd"  node:"HugePages_Rsvd"     m:"1"    help:"The number of committed huge pages."`
	HugePagesSize  int `json:"hugePagesSize"  node:"Hugepagesize_bytes" m:"1024" help:"The size for each huge pages unit, in kilobytes."`
	HugePagesSurp  int `json:"hugePagesSurp"  node:"HugePages_Surp"     m:"1"    help:"The number of available surplus huge pages over the total."`
	HugePagesTotal int `json:"hugePagesTotal" node:"HugePages_Total"    m:"1"    help:"The total number of huge pages for the system."`
	Inactive       int `json:"inactive"       node:"Inactive_bytes"     m:"1024" help:"The amount of least-frequently used memory pages, in kilobytes."`
	Mapped         int `json:"mapped"         node:"Mapped_bytes"       m:"1024" help:"The total amount of file-system contents that is memory mapped inside a process address space, in kilobytes."`
	PageTables     int `json:"pageTables"     node:"PageTables_bytes"   m:"1024" help:"The amount of memory used by page tables, in kilobytes."`
	Slab           int `json:"slab"           node:"Slab_bytes"         m:"1024" help:"The amount of reusable kernel data structures, in kilobytes."`
	Total          int `json:"total"          node:"MemTotal_bytes"     m:"1024" help:"The total amount of memory, in kilobytes."`
	Writeback      int `json:"writeback"      node:"Writeback_bytes"    m:"1024" help:"The amount of dirty pages in RAM that are still being written to the backing storage, in kilobytes."`
}

type network struct {
	Interface string  `json:"interface" help:"The identifier for the network interface being used for the DB instance."`
	Rx        float64 `json:"rx"        help:"The number of bytes received per second."`
	Tx        float64 `json:"tx"        help:"The number of bytes uploaded per second."`
}

//nolint:lll
type processList struct {
	CPUUsedPC    float64 `json:"cpuUsedPc"    help:"The percentage of CPU used by the process."`
	ID           int     `json:"id"           help:"The identifier of the process."`
	MemoryUsedPC float64 `json:"memoryUsedPc" help:"The amount of memory used by the process, in kilobytes."`
	Name         string  `json:"name"         help:"The name of the process."`
	ParentID     int     `json:"parentID"     help:"The process identifier for the parent process of the process."`
	RSS          int     `json:"rss"          help:"The amount of RAM allocated to the process, in kilobytes."`
	TGID         int     `json:"tgid"         help:"The thread group identifier, which is a number representing the process ID to which a thread belongs. This identifier is used to group threads from the same process."`
	VSS          int     `json:"vss"          help:"The amount of virtual memory allocated to the process, in kilobytes."`

	// TODO Handle this.
	VMLimit interface{} `json:"vmlimit" help:"-"`
}

//nolint:lll
type swap struct {
	Cached float64 `json:"cached" node:"node_memory_SwapCached_bytes" m:"1024" help:"The amount of swap memory, in kilobytes, used as cache memory."  nodehelp:"Memory information field SwapCached."`
	Free   float64 `json:"free"   node:"node_memory_SwapFree_bytes"   m:"1024" help:"The total amount of swap memory free, in kilobytes."             nodehelp:"Memory information field SwapFree."`
	Total  float64 `json:"total"  node:"node_memory_SwapTotal_bytes"  m:"1024" help:"The total amount of swap memory available, in kilobytes."        nodehelp:"Memory information field SwapTotal."`

	// we use multiplier 0.25 to convert a number of kilobytes to a number of 4k pages (what our dashboards assume)
	In  float64 `json:"in"  node:"node_vmstat_pswpin"  m:"0.25" help:"The total amount of memory, in kilobytes, swapped in from disk." nodehelp:"/proc/vmstat information field pswpin"`
	Out float64 `json:"out" node:"node_vmstat_pswpout" m:"0.25" help:"The total amount of memory, in kilobytes, swapped out to disk."  nodehelp:"/proc/vmstat information field pswpout"`
}

type tasks struct {
	Blocked  int `json:"blocked"  help:"The number of tasks that are blocked."`
	Running  int `json:"running"  help:"The number of tasks that are running."`
	Sleeping int `json:"sleeping" help:"The number of tasks that are sleeping."`
	Stopped  int `json:"stopped"  help:"The number of tasks that are stopped."`
	Total    int `json:"total"    help:"The total number of tasks."`
	Zombie   int `json:"zombie"   help:"The number of child tasks that are inactive with an active parent task."`
}

// parseOSMetrics parses OS metrics from given JSON data.
func parseOSMetrics(b []byte, disallowUnknownFields bool) (*osMetrics, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	if disallowUnknownFields {
		d.DisallowUnknownFields()
	}

	var m osMetrics
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// makeGauge returns Prometheus gauge for given reflect.Value.
func makeGauge(desc *prometheus.Desc, labelValues []string, value reflect.Value) prometheus.Metric {
	// skip nil fields
	if value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}

	var f float64
	switch kind := value.Kind(); kind {
	case reflect.Float64:
		f = value.Float()
	case reflect.Int, reflect.Int64:
		f = float64(value.Int())
	default:
		panic(fmt.Errorf("can't make a metric value for %s from %v (%s)", desc, value, kind))
	}

	return prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, f, labelValues...)
}

// makeGenericMetrics makes metrics for all structure fields.
func makeGenericMetrics(s interface{}, namePrefix string, constLabels prometheus.Labels) []prometheus.Metric {
	t := reflect.TypeOf(s)
	v := reflect.ValueOf(s)
	res := make([]prometheus.Metric, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tags := t.Field(i).Tag
		name, help := tags.Get("json"), tags.Get("help")
		desc := prometheus.NewDesc(namePrefix+name, help, nil, constLabels)
		m := makeGauge(desc, nil, v.Field(i))
		if m != nil {
			res = append(res, m)
		}
	}
	return res
}

// makeNodeCPUMetrics returns node_exporter-like node_cpu_average metrics.
func makeNodeCPUMetrics(s *cpuUtilization, constLabels prometheus.Labels) []prometheus.Metric {
	// add "cpu" constant label
	labels := prometheus.Labels{"cpu": "All"}
	for k, v := range constLabels {
		labels[k] = v
	}

	// move mode to label
	t := reflect.TypeOf(*s)
	v := reflect.ValueOf(*s)
	res := make([]prometheus.Metric, 0, t.NumField())
	desc := prometheus.NewDesc("node_cpu_average", "The percentage of CPU utilization.", []string{"mode"}, labels)
	for i := 0; i < t.NumField(); i++ {
		tags := t.Field(i).Tag
		mode := tags.Get("json")
		m := makeGauge(desc, []string{mode}, v.Field(i))
		if m != nil {
			res = append(res, m)
		}
	}
	return res
}

// makeRDSDiskIOMetrics returns rdsosmetrics_diskIO_ metrics.
func makeRDSDiskIOMetrics(s *diskIO, constLabels prometheus.Labels) []prometheus.Metric {
	// move device name to label
	labelKeys := []string{"device"}
	labelValues := []string{s.Device}

	t := reflect.TypeOf(*s)
	v := reflect.ValueOf(*s)
	res := make([]prometheus.Metric, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tags := t.Field(i).Tag
		name, help := tags.Get("json"), tags.Get("help")
		if name == "device" {
			continue
		}
		desc := prometheus.NewDesc("rdsosmetrics_diskIO_"+name, help, labelKeys, constLabels)
		m := makeGauge(desc, labelValues, v.Field(i))
		if m != nil {
			res = append(res, m)
		}
	}
	return res
}

// makeNodeDiskMetrics returns node_exporter-like node_disk_ metrics.
func makeNodeDiskMetrics(s *diskIO, constLabels prometheus.Labels) []prometheus.Metric {
	// move device name to label
	labelKeys := []string{"device"}
	labelValues := []string{s.Device}
	res := make([]prometheus.Metric, 0, 2)

	if s.ReadKb != nil {
		desc := prometheus.NewDesc("node_disk_read_bytes_total", "The total number of bytes read successfully.", labelKeys, constLabels)
		m := prometheus.MustNewConstMetric(desc, prometheus.CounterValue, float64(*s.ReadKb*1024), labelValues...)
		res = append(res, m)
	}
	if s.WriteKb != nil {
		desc := prometheus.NewDesc("node_disk_written_bytes_total", "The total number of bytes written successfully.", labelKeys, constLabels)
		m := prometheus.MustNewConstMetric(desc, prometheus.CounterValue, float64(*s.WriteKb*1024), labelValues...)
		res = append(res, m)
	}

	return res
}

// makeRDSFileSysMetrics returns rdsosmetrics_fileSys_ metrics.
func makeRDSFileSysMetrics(s *fileSys, constLabels prometheus.Labels) []prometheus.Metric {
	// move name and mount point to labels
	labelKeys := []string{"name", "mount_point"}
	labelValues := []string{s.Name, s.MountPoint}

	t := reflect.TypeOf(*s)
	v := reflect.ValueOf(*s)
	res := make([]prometheus.Metric, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tags := t.Field(i).Tag
		name, help := tags.Get("json"), tags.Get("help")
		switch name {
		case "name", "mountPoint":
			continue
		}
		desc := prometheus.NewDesc("rdsosmetrics_fileSys_"+name, help, labelKeys, constLabels)
		m := makeGauge(desc, labelValues, v.Field(i))
		if m != nil {
			res = append(res, m)
		}
	}
	return res
}

// makeNodeFilesystemMetrics returns node_exporter-like node_filesystem_ metrics.
func makeNodeFilesystemMetrics(s *fileSys, constLabels prometheus.Labels) []prometheus.Metric {
	// all labels are used by our dashboards, so fill both device and fstype with the same value
	labelKeys := []string{"device", "fstype", "mountpoint"}
	labelValues := []string{s.Name, s.Name, s.MountPoint}
	res := make([]prometheus.Metric, 0, 5)

	desc := prometheus.NewDesc("node_filesystem_files", "Filesystem total file nodes.", labelKeys, constLabels)
	res = append(res, prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(s.MaxFiles*1024), labelValues...))
	desc = prometheus.NewDesc("node_filesystem_files_free", "Filesystem total free file nodes.", labelKeys, constLabels)
	res = append(res, prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64((s.MaxFiles-s.UsedFiles)*1024), labelValues...))

	// report the same value for node_filesystem_free and node_filesystem_avail because we use both metrics in our dashboards
	desc = prometheus.NewDesc("node_filesystem_size_bytes", "Filesystem size in bytes.", labelKeys, constLabels)
	res = append(res, prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(s.Total*1024), labelValues...))
	desc = prometheus.NewDesc("node_filesystem_free_bytes", "Filesystem free space in bytes.", labelKeys, constLabels)
	res = append(res, prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64((s.Total-s.Used)*1024), labelValues...))
	desc = prometheus.NewDesc("node_filesystem_avail_bytes", "Filesystem space available to non-root users in bytes.", labelKeys, constLabels)
	res = append(res, prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64((s.Total-s.Used)*1024), labelValues...))

	return res
}

// makeNodeLoadMetrics returns node_exporter-like node_load1 metric.
func makeNodeLoadMetrics(s *loadAverageMinute, constLabels prometheus.Labels) []prometheus.Metric {
	desc := prometheus.NewDesc("node_load1", "The number of processes requesting CPU time over the last minute.", nil, constLabels)
	m := prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, s.One)
	return []prometheus.Metric{m}
}

// makeNodeMemoryMetrics returns node_exporter-like node_memory_ metrics.
func makeNodeMemoryMetrics(s *memory, constLabels prometheus.Labels) []prometheus.Metric {
	t := reflect.TypeOf(*s)
	v := reflect.ValueOf(*s)
	res := make([]prometheus.Metric, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tags := t.Field(i).Tag
		suffix, multiplierS := tags.Get("node"), tags.Get("m")
		multiplier, err := strconv.ParseInt(multiplierS, 10, 64)
		if err != nil {
			panic(err)
		}
		desc := prometheus.NewDesc("node_memory_"+suffix, "Memory information field "+suffix+".", nil, constLabels)
		m := makeGauge(desc, nil, reflect.ValueOf(v.Field(i).Int()*multiplier))
		if m != nil {
			res = append(res, m)
		}
	}
	return res
}

// makeRDSNetworkMetrics returns rdsosmetrics_network_ metrics.
func makeRDSNetworkMetrics(s *network, constLabels prometheus.Labels) []prometheus.Metric {
	// move interface name to label
	labelKeys := []string{"interface"}
	labelValues := []string{s.Interface}

	t := reflect.TypeOf(*s)
	v := reflect.ValueOf(*s)
	res := make([]prometheus.Metric, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tags := t.Field(i).Tag
		name, help := tags.Get("json"), tags.Get("help")
		if name == "interface" {
			continue
		}
		desc := prometheus.NewDesc("rdsosmetrics_network_"+name, help, labelKeys, constLabels)
		m := makeGauge(desc, labelValues, v.Field(i))
		if m != nil {
			res = append(res, m)
		}
	}
	return res
}

// makeRDSProcessListMetrics returns rdsosmetrics_processList_ metrics.
func makeRDSProcessListMetrics(s *processList, constLabels prometheus.Labels) []prometheus.Metric {
	// move process name, ID, parent ID, thread ID to labels
	labelKeys := []string{"name", "id", "parentID", "tgid"}
	labelValues := []string{s.Name, strconv.Itoa(s.ID), strconv.Itoa(s.ParentID), strconv.Itoa(s.TGID)}

	t := reflect.TypeOf(*s)
	v := reflect.ValueOf(*s)
	res := make([]prometheus.Metric, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tags := t.Field(i).Tag
		name, help := tags.Get("json"), tags.Get("help")
		if help == "-" {
			continue
		}

		switch name {
		case "name", "id", "parentID", "tgid":
			continue
		}
		desc := prometheus.NewDesc("rdsosmetrics_processList_"+name, help, labelKeys, constLabels)
		m := makeGauge(desc, labelValues, v.Field(i))
		if m != nil {
			res = append(res, m)
		}
	}
	return res
}

// makeNodeMemorySwapMetrics returns node_exporter-like node_memory_ metrics for swap.
func makeNodeMemorySwapMetrics(s *swap, constLabels prometheus.Labels) []prometheus.Metric {
	t := reflect.TypeOf(*s)
	v := reflect.ValueOf(*s)
	res := make([]prometheus.Metric, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tags := t.Field(i).Tag
		name, multiplierS, help := tags.Get("node"), tags.Get("m"), tags.Get("nodehelp")
		multiplier, err := strconv.ParseFloat(multiplierS, 64)
		if err != nil {
			panic(err)
		}
		desc := prometheus.NewDesc(name, help, nil, constLabels)
		m := makeGauge(desc, nil, reflect.ValueOf(v.Field(i).Float()*multiplier))
		if m != nil {
			res = append(res, m)
		}
	}
	return res
}

// makeNodeProcsMetrics returns node_exporter-like node_procs_ metrics.
func makeNodeProcsMetrics(s *tasks, constLabels prometheus.Labels) []prometheus.Metric {
	res := make([]prometheus.Metric, 0, 2)
	desc := prometheus.NewDesc("node_procs_blocked", "Number of processes blocked waiting for I/O to complete.", nil, constLabels)
	res = append(res, prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(s.Blocked)))
	desc = prometheus.NewDesc("node_procs_running", "Number of processes in runnable state.", nil, constLabels)
	res = append(res, prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(s.Running)))
	return res
}

// makePrometheusMetrics returns all Prometheus metrics for given osMetrics.
func (m *osMetrics) makePrometheusMetrics(region string, labels map[string]string) []prometheus.Metric {
	res := make([]prometheus.Metric, 0, 100)

	constLabels := prometheus.Labels{
		"region":   region,
		"instance": m.InstanceID,
	}
	for n, v := range labels {
		if v == "" {
			delete(constLabels, n)
		} else {
			constLabels[n] = v
		}
	}

	res = append(res, prometheus.MustNewConstMetric(
		prometheus.NewDesc("rdsosmetrics_timestamp", "Metrics timestamp (UNIX seconds).", nil, constLabels),
		prometheus.CounterValue,
		float64(m.Timestamp.Unix()),
	))

	res = append(res, prometheus.MustNewConstMetric(
		prometheus.NewDesc("rdsosmetrics_General_numVCPUs", "The number of virtual CPUs for the DB instance.", nil, constLabels),
		prometheus.GaugeValue,
		float64(m.NumVCPUs)),
	)

	// always make both generic and node_exporter-like metrics

	metrics := makeGenericMetrics(m.CPUUtilization, "rdsosmetrics_cpuUtilization_", constLabels)
	res = append(res, metrics...)
	metrics = makeNodeCPUMetrics(&m.CPUUtilization, constLabels)
	res = append(res, metrics...)

	for _, disk := range m.DiskIO {
		metrics = makeRDSDiskIOMetrics(&disk, constLabels)
		res = append(res, metrics...)
		metrics = makeNodeDiskMetrics(&disk, constLabels)
		res = append(res, metrics...)
	}

	for _, fs := range m.FileSys {
		metrics = makeRDSFileSysMetrics(&fs, constLabels)
		res = append(res, metrics...)
		metrics = makeNodeFilesystemMetrics(&fs, constLabels)
		res = append(res, metrics...)
	}

	metrics = makeGenericMetrics(m.LoadAverageMinute, "rdsosmetrics_loadAverageMinute_", constLabels)
	res = append(res, metrics...)
	metrics = makeNodeLoadMetrics(&m.LoadAverageMinute, constLabels)
	res = append(res, metrics...)

	metrics = makeGenericMetrics(m.Memory, "rdsosmetrics_memory_", constLabels)
	res = append(res, metrics...)
	metrics = makeNodeMemoryMetrics(&m.Memory, constLabels)
	res = append(res, metrics...)

	for _, n := range m.Network {
		metrics = makeRDSNetworkMetrics(&n, constLabels)
		res = append(res, metrics...)
		// we can't make node_exporter-like metrics: AWS gives us rates, node_exporter - total counters
	}

	for _, p := range m.ProcessList {
		metrics = makeRDSProcessListMetrics(&p, constLabels)
		res = append(res, metrics...)
		// no node_exporter-like metrics
	}

	metrics = makeGenericMetrics(m.Swap, "rdsosmetrics_swap_", constLabels)
	res = append(res, metrics...)
	metrics = makeNodeMemorySwapMetrics(&m.Swap, constLabels)
	res = append(res, metrics...)

	metrics = makeGenericMetrics(m.Tasks, "rdsosmetrics_tasks_", constLabels)
	res = append(res, metrics...)
	metrics = makeNodeProcsMetrics(&m.Tasks, constLabels)
	res = append(res, metrics...)

	return res
}
