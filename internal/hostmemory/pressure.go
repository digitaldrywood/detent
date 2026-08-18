package hostmemory

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var ErrUnsupported = errors.New("host memory pressure is unsupported on this platform")

type Sample struct {
	Some       Pressure
	Full       Pressure
	ObservedAt time.Time
}

type Pressure struct {
	Avg10  float64
	Avg60  float64
	Avg300 float64
	Total  uint64
}

func Parse(value string) (Sample, error) {
	var sample Sample
	seen := map[string]bool{}
	for line := range strings.Lines(value) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		kind := fields[0]
		if kind != "some" && kind != "full" {
			continue
		}
		pressure, err := parsePressureFields(fields[1:])
		if err != nil {
			return Sample{}, fmt.Errorf("parse memory pressure %s: %w", kind, err)
		}
		if kind == "some" {
			sample.Some = pressure
		} else {
			sample.Full = pressure
		}
		seen[kind] = true
	}
	if !seen["some"] {
		return Sample{}, errors.New("parse memory pressure: some row is missing")
	}
	return sample, nil
}

func parsePressureFields(fields []string) (Pressure, error) {
	values := make(map[string]string, len(fields))
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if ok {
			values[key] = value
		}
	}
	avg10, err := parseFloat(values, "avg10")
	if err != nil {
		return Pressure{}, err
	}
	avg60, err := parseFloat(values, "avg60")
	if err != nil {
		return Pressure{}, err
	}
	avg300, err := parseFloat(values, "avg300")
	if err != nil {
		return Pressure{}, err
	}
	total, err := strconv.ParseUint(values["total"], 10, 64)
	if err != nil {
		return Pressure{}, fmt.Errorf("total: %w", err)
	}
	return Pressure{Avg10: avg10, Avg60: avg60, Avg300: avg300, Total: total}, nil
}

func parseFloat(values map[string]string, key string) (float64, error) {
	value, err := strconv.ParseFloat(values[key], 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return value, nil
}
