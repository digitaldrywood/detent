package dispatch

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
)

const finalHostMetricsTimeout = 3 * time.Second

func (s *Scheduler) RunHostMetrics(ctx context.Context, root string, started time.Time) {
	if !s.client.Enrolled() {
		return
	}
	metrics := hostmetrics.New(root, started)
	s.mu.Lock()
	s.hostMetrics = metrics
	s.mu.Unlock()
	metrics.Run(ctx)
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalHostMetricsTimeout)
	defer cancel()
	if err := s.flushHostMetrics(finalCtx, metrics.Close(time.Now())); err != nil {
		slog.Default().Warn("final runner host metrics heartbeat failed", "error", err)
	}
}

func (s *Scheduler) acknowledgeHostMetrics(metrics *hostmetrics.Collector, summaries []hostmetrics.Summary, acknowledged []hostmetrics.Acknowledgment) {
	if metrics == nil {
		return
	}
	sent := slices.DeleteFunc(slices.Clone(summaries), func(summary hostmetrics.Summary) bool {
		return !slices.ContainsFunc(acknowledged, func(ack hostmetrics.Acknowledgment) bool {
			return ack.Hour.Equal(summary.Hour) && ack.SegmentID.Equal(summary.SegmentID)
		})
	})
	metrics.Acknowledge(sent)
}

func (s *Scheduler) sendNativeMachineHeartbeat(ctx context.Context, source *NativeConnector, machine Machine, register bool) error {
	s.controlMu.RLock()
	defer s.controlMu.RUnlock()
	if s.controlPaused.Load() {
		return nil
	}
	s.hostMetricsMu.Lock()
	defer s.hostMetricsMu.Unlock()
	s.mu.Lock()
	metrics := s.hostMetrics
	s.mu.Unlock()
	if metrics != nil {
		machine.HostMetrics = metrics.Summaries()
	}
	var acknowledged []hostmetrics.Acknowledgment
	machine.HostMetricsAcknowledged = &acknowledged
	var err error
	if register {
		err = source.NativeClient().RegisterMachine(ctx, machine)
	} else {
		err = source.NativeClient().HeartbeatMachine(ctx, machine)
	}
	if err == nil {
		s.acknowledgeHostMetrics(metrics, machine.HostMetrics, acknowledged)
	}
	return err
}

func (s *Scheduler) flushHostMetrics(ctx context.Context, summaries []hostmetrics.Summary) error {
	s.controlMu.RLock()
	defer s.controlMu.RUnlock()
	if s.controlPaused.Load() {
		return nil
	}
	if len(summaries) == 0 || !s.client.Enrolled() {
		return nil
	}
	s.hostMetricsMu.Lock()
	defer s.hostMetricsMu.Unlock()
	acknowledged, err := s.client.FlushHostMetrics(ctx, summaries)
	if err != nil {
		return err
	}
	s.mu.Lock()
	metrics := s.hostMetrics
	s.mu.Unlock()
	s.acknowledgeHostMetrics(metrics, summaries, acknowledged)
	return nil
}
