package hubclient

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
)

const finalHostMetricsTimeout = 3 * time.Second

func (s *Scheduler) RunHostMetrics(ctx context.Context, root string, started time.Time) {
	if s.client.runner == nil {
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

func (s *Scheduler) flushHostMetrics(ctx context.Context, summaries []hostmetrics.Summary) error {
	if len(summaries) == 0 {
		return nil
	}
	s.mu.Lock()
	machine := s.machine
	s.mu.Unlock()
	machine.HostMetrics = summaries
	var rejected bool
	var previous nativeMachineHeartbeat
	var path string
	if s.client.runner != nil {
		s.client.runner.routingMu.Lock()
		machine.Problems = slices.Clone(s.client.runner.problems)
		rejected = s.client.runner.settingsRejected
		previous, path = s.client.runner.heartbeat, s.client.runner.heartbeatPath
		s.client.runner.routingMu.Unlock()
	}
	if path != "" {
		previous.HostMetrics = summaries
		previous.Problems, previous.SettingsRejected = machine.Problems, rejected
		return s.client.request(ctx, http.MethodPost, path, previous, nil)
	}
	for _, source := range s.nativeProjectSnapshot() {
		return source.client.client.request(ctx, http.MethodPost, source.client.base()+"/machines/"+url.PathEscape(string(machine.ID))+"/heartbeat", machineHeartbeatPayload(machine, machine.Problems, rejected), nil)
	}
	return nil
}
