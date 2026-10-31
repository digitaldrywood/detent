package hostmetrics

import (
	"context"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
)

func readHost(ctx context.Context, root string) reading {
	var sample reading
	if memory, err := mem.VirtualMemoryWithContext(ctx); err == nil && memory != nil {
		sample.memoryTotal, sample.memoryAvailable, sample.memoryOK = memory.Total, memory.Available, true
	}
	if swap, err := mem.SwapMemoryWithContext(ctx); err == nil && swap != nil {
		sample.swapUsed, sample.swapOK = swap.Used, true
	}
	if counters, err := cpu.TimesWithContext(ctx, false); err == nil && len(counters) == 1 {
		times := counters[0]
		sample.cpu = cpuCounters{
			total: times.User + times.Nice + times.System + times.Idle + times.Iowait + times.Irq + times.Softirq + times.Steal,
			idle:  times.Idle + times.Iowait,
		}
		sample.cpuOK = true
	}
	if average, err := load.AvgWithContext(ctx); err == nil && average != nil {
		sample.load1, sample.loadOK = average.Load1, true
	}
	sample.diskFree, sample.diskTotal, sample.diskOK = readDisk(root)
	readPressure(ctx, &sample)
	return sample
}
