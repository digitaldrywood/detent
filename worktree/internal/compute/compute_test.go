package compute

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"testing/synctest"
	"time"
)

func TestCalculate(t *testing.T) {
	t.Parallel()
	zero := 0.0
	cpuPrice, memoryPrice := 2.0, 3.0
	base := time.Unix(0, 0)
	for _, tt := range []struct {
		name  string
		cpu   uint64
		wall  time.Duration
		bytes float64
		rates Rates
		want  *Usage
	}{
		{"default rates", 3_600_000_000, time.Hour, 2e9 * 3600, Rates{}, &Usage{3600, 2e9, 3600, .1575}},
		{"fractional interval", 500_000, 250 * time.Millisecond, 1e9 * .25, Rates{}, &Usage{.5, 1e9, .25, (.5*.07 + .25*.04375) / 3600}},
		{"override rates", 1_800_000_000, time.Hour, 1e9 * 3600, Rates{&cpuPrice, &memoryPrice}, &Usage{1800, 1e9, 3600, 4}},
		{"zero rates", 1_000_000, time.Second, 1e9, Rates{&zero, &zero}, &Usage{1, 1e9, 1, 0}},
		{"zero duration", 1, 0, 0, Rates{}, nil},
		{"counter reset", 0, time.Second, 1e9, Rates{}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			first := sample{cpu: 1, at: base}
			last := sample{cpu: tt.cpu + 1, at: base.Add(tt.wall)}
			if tt.name == "counter reset" {
				last.cpu = 0
			}
			got := calculate(first, last, tt.bytes, tt.rates)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got %+v, want unavailable", got)
				}
				return
			}
			if got == nil || math.Abs(got.ComputeUSD-tt.want.ComputeUSD) > 1e-12 || *got != (Usage{tt.want.CPUSeconds, tt.want.AvgMemoryBytes, tt.want.WallSeconds, got.ComputeUSD}) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestMeter(t *testing.T) {
	for _, tt := range []struct {
		name, cpu, memory string
		missing           string
		failAfter         bool
		want              bool
	}{
		{name: "samples and partial last interval", cpu: "usage_usec 100\nuser_usec 99", memory: "1000000000", want: true},
		{name: "no cgroup", missing: "cpu.stat"},
		{name: "cgroup v1", cpu: "user_usec 100", memory: "1"},
		{name: "missing memory controller", cpu: "usage_usec 100", missing: "memory.current"},
		{name: "malformed cpu", cpu: "usage_usec oops", memory: "1"},
		{name: "malformed memory", cpu: "usage_usec 100", memory: "oops"},
		{name: "counter read fails during turn", cpu: "usage_usec 100", memory: "1", failAfter: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				base := time.Now()
				stop := start(Rates{}, func(name string) ([]byte, error) {
					elapsed := time.Since(base)
					if name == tt.missing || (tt.failAfter && elapsed > 0) {
						return nil, errors.New("unavailable")
					}
					if name == "cpu.stat" {
						if tt.want {
							return fmt.Appendf(nil, "usage_usec %d", 100+elapsed.Microseconds()), nil
						}
						return []byte(tt.cpu), nil
					}
					if tt.want {
						return fmt.Appendf(nil, "%d", (int64(elapsed/time.Second)+1)*1_000_000_000), nil
					}
					return []byte(tt.memory), nil
				}, time.Now)
				time.Sleep(2500 * time.Millisecond)
				got := stop()
				if !tt.want {
					if got != nil {
						t.Fatalf("got %+v, want unavailable", got)
					}
					return
				}
				if got == nil || got.CPUSeconds != 2.5 || got.WallSeconds != 2.5 || got.AvgMemoryBytes != 1.8e9 {
					t.Fatalf("usage = %+v, want weighted samples 1s @ 1GB, 1s @ 2GB, .5s @ 3GB", got)
				}
			})
		})
	}
}

func TestAdd(t *testing.T) {
	t.Parallel()
	a, b := &Usage{1, 1e9, 1, .1}, &Usage{2, 3e9, 3, .2}
	for _, tt := range []struct {
		name string
		a, b *Usage
		want bool
	}{{"measured", a, b, true}, {"missing first", nil, b, false}, {"missing second", a, nil, false}} {
		t.Run(tt.name, func(t *testing.T) {
			got := Add(tt.a, tt.b)
			if !tt.want {
				if got != nil {
					t.Fatal("partial measurement reported as complete")
				}
				return
			}
			if got.CPUSeconds != 3 || got.WallSeconds != 4 || got.AvgMemoryBytes != 2.5e9 || math.Abs(got.ComputeUSD-.3) > 1e-12 {
				t.Fatalf("sum = %+v", got)
			}
		})
	}
}
