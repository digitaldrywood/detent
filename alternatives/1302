package hostmetrics

import "time"

type Summary struct {
	Hour                    time.Time `json:"hour"`
	SegmentID               time.Time `json:"segment_id"`
	Partial                 bool      `json:"partial,omitempty"`
	SampleCount             uint64    `json:"sample_count"`
	LogicalCores            int       `json:"logical_cores"`
	MemoryTotalBytes        uint64    `json:"memory_total_bytes"`
	MemoryAvailableMinBytes uint64    `json:"memory_available_min_bytes"`
	MemoryAvailableSumBytes uint64    `json:"memory_available_sum_bytes"`
	MemorySampleCount       uint64    `json:"memory_sample_count"`
	SwapUsedMaxBytes        uint64    `json:"swap_used_max_bytes"`
	SwapSampleCount         uint64    `json:"swap_sample_count"`
	PSISomeAvg10Sum         float64   `json:"psi_some_avg10_sum"`
	PSISomeSampleCount      uint64    `json:"psi_some_sample_count"`
	PSIFullAvg10Sum         float64   `json:"psi_full_avg10_sum"`
	PSIFullSampleCount      uint64    `json:"psi_full_sample_count"`
	PressureWarnCount       uint64    `json:"pressure_warn_count"`
	PressureCriticalCount   uint64    `json:"pressure_critical_count"`
	PressureSampleCount     uint64    `json:"pressure_sample_count"`
	CPUBusySumPercent       float64   `json:"cpu_busy_sum_percent"`
	CPUBusyMaxPercent       float64   `json:"cpu_busy_max_percent"`
	CPUSampleCount          uint64    `json:"cpu_sample_count"`
	Load1Max                float64   `json:"load1_max"`
	LoadSampleCount         uint64    `json:"load_sample_count"`
	DiskFreeMinBytes        uint64    `json:"disk_free_min_bytes"`
	DiskTotalMinBytes       uint64    `json:"disk_total_min_bytes"`
	DiskSampleCount         uint64    `json:"disk_sample_count"`
}

type Acknowledgment struct {
	Hour      time.Time `json:"hour"`
	SegmentID time.Time `json:"segment_id"`
}
