// Package compute meters consumed resources in the worker's cgroup v2.
package compute

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Rates are USD per consumed CPU-hour and memory GB-hour (decimal GB).
// Nil prices inherit Sprites public rates; explicit zero means free compute.
type Rates struct {
	CPUHourUSD      *float64 `yaml:"cpu_hour_usd,omitempty" json:"cpu_hour_usd,omitempty"`
	MemoryGBHourUSD *float64 `yaml:"memory_gb_hour_usd,omitempty" json:"memory_gb_hour_usd,omitempty"`
}

func (r Rates) prices() (float64, float64) {
	cpu, memory := 0.07, 0.04375
	if r.CPUHourUSD != nil {
		cpu = *r.CPUHourUSD
	}
	if r.MemoryGBHourUSD != nil {
		memory = *r.MemoryGBHourUSD
	}
	return cpu, memory
}

func (r Rates) Validate() error {
	cpu, memory := r.prices()
	for _, price := range []float64{cpu, memory} {
		if price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			return fmt.Errorf("compute rates must be finite and nonnegative")
		}
	}
	return nil
}

type Usage struct {
	CPUSeconds     float64 `json:"cpu_seconds"`
	AvgMemoryBytes float64 `json:"avg_memory_bytes"`
	WallSeconds    float64 `json:"wall_seconds"`
	ComputeUSD     float64 `json:"compute_usd"`
}

// Add combines measured turns. An unavailable segment makes the total unknown.
func Add(a, b *Usage) *Usage {
	if a == nil || b == nil {
		return nil
	}
	wall := a.WallSeconds + b.WallSeconds
	average := 0.0
	if wall > 0 {
		average = (a.AvgMemoryBytes*a.WallSeconds + b.AvgMemoryBytes*b.WallSeconds) / wall
	}
	return &Usage{CPUSeconds: a.CPUSeconds + b.CPUSeconds, AvgMemoryBytes: average, WallSeconds: wall, ComputeUSD: a.ComputeUSD + b.ComputeUSD}
}

// Start returns a stop function. Missing cgroup v2 counters produce nil usage;
// metering never changes the turn's outcome. Stop must be called once, after
// the agent returns, including on failure or cancellation.
func Start(rates Rates) func() *Usage {
	if runtime.GOOS != "linux" {
		return func() *Usage { return nil }
	}
	root := "/sys/fs/cgroup"
	// On ordinary Linux hosts the runner can be in a nested cgroup. In a
	// cgroup namespace (including Sprites), the unified membership is '/'.
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return func() *Usage { return nil }
	}
	found := false
	for line := range strings.SplitSeq(string(data), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			root = filepath.Join(root, filepath.Clean("/"+path))
			found = true
			break
		}
	}
	if !found {
		return func() *Usage { return nil }
	}
	return start(rates, func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(root, name)) }, time.Now)
}

type sample struct {
	cpu    uint64
	memory float64
	at     time.Time
}

func readSample(read func(string) ([]byte, error), now func() time.Time) (sample, error) {
	data, err := read("cpu.stat")
	if err != nil {
		return sample{}, err
	}
	var cpu uint64
	found := false
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			cpu, err = strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return sample{}, err
			}
			found = true
			break
		}
	}
	if !found {
		return sample{}, fmt.Errorf("cpu.stat has no usage_usec")
	}
	data, err = read("memory.current")
	if err != nil {
		return sample{}, err
	}
	memory, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return sample{}, err
	}
	return sample{cpu: cpu, memory: float64(memory), at: now()}, nil
}

func start(rates Rates, read func(string) ([]byte, error), now func() time.Time) func() *Usage {
	first, err := readSample(read, now)
	if err != nil || rates.Validate() != nil {
		return func() *Usage { return nil }
	}
	stop := make(chan struct{})
	done := make(chan *Usage, 1)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		previous := first
		byteSeconds := 0.0
		valid := true
		for {
			finished := false
			select {
			case <-ticker.C:
			case <-stop:
				finished = true
			}
			next, err := readSample(read, now)
			if err != nil || next.cpu < previous.cpu || next.at.Before(previous.at) {
				valid = false
			}
			if valid {
				byteSeconds += previous.memory * next.at.Sub(previous.at).Seconds()
			}
			previous = next
			if finished {
				if !valid {
					done <- nil
				} else {
					done <- calculate(first, next, byteSeconds, rates)
				}
				return
			}
		}
	}()
	return func() *Usage { close(stop); return <-done }
}

func calculate(first, last sample, byteSeconds float64, rates Rates) *Usage {
	wall := last.at.Sub(first.at).Seconds()
	if wall <= 0 || last.cpu < first.cpu {
		return nil
	}
	cpu := float64(last.cpu-first.cpu) / 1e6
	cpuRate, memoryRate := rates.prices()
	return &Usage{CPUSeconds: cpu, AvgMemoryBytes: byteSeconds / wall, WallSeconds: wall, ComputeUSD: cpu/3600*cpuRate + byteSeconds/1e9/3600*memoryRate}
}
