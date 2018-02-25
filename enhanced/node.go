package enhanced

func MapToNode(subsystem, name string, extraLabelsValues ...string) (namespaceOUT, subsystemOUT, nameOUT string, extraLabelsOUT []string, extraLabelsValuesOUT []string) {
	switch subsystem {
	case "cpuUtilization":
		// map cpuUtilization to node_cpu{cpu="All"}

		// Turn metric name to 'mode' label e.g. node_cpu{cpu="All", mode="nice"}
		return "node", "", "cpu_average", []string{"mode"}, []string{name}
	case "loadAverageMinute":
		// map loadAverageMinute.one to node_load1
		switch name {
		case "one":
			return "node", "", "load1", nil, nil
		}
	case "memory":
		names := map[string]string{
			"buffers":    "Buffers",
			"cached":     "Cached",
			"free":       "MemFree",
			"total":      "MemTotal",
			"active":     "Active",
			"inactive":   "Inactive",
			"slab":       "Slab",
			"mapped":     "Mapped",
			"pageTables": "PageTables",
			"dirty":      "nr_dirty",
		}
		if nodeName, ok := names[name]; ok {
			return "node", "memory", nodeName, nil, nil
		}
	case "swap":
		names := map[string]string{
			"free":  "SwapFree",
			"total": "SwapTotal",
		}
		if nodeName, ok := names[name]; ok {
			return "node", "memory", nodeName, nil, nil
		}
		names = map[string]string{
			"in":  "pswpin",
			"out": "pswpout",
		}
		if nodeName, ok := names[name]; ok {
			return "node", "vmstat", nodeName, nil, nil
		}
	case "tasks":
		switch name {
		case "blocked",
			"running":
			return "node", "procs", name, nil, nil
		}
	case "fileSys":
		names := map[string]string{
			"name":  "avail",
			"total": "size",
		}
		if nodeName, ok := names[name]; ok {
			return "node", "filesystem", nodeName, nil, nil
		}
	case "diskIO":
		// Only first device is converted to node name
		if len(extraLabelsValues) == 1 && extraLabelsValues[0] == "0" {
			names := map[string]string{
				"readKb":  "bytes_read",
				"writeKb": "bytes_written",
			}
			if nodeName, ok := names[name]; ok {
				return "node", "disk", nodeName, nil, nil
			}
		}
		return defaultNamespace, subsystem, name, []string{"id"}, extraLabelsValues
	case "processList",
		"network":
		return defaultNamespace, subsystem, name, []string{"id"}, extraLabelsValues
	}

	// If can't be mapped to node, then return original
	return defaultNamespace, subsystem, name, nil, nil
}
