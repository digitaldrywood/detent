package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/githuboutbound"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) githubOutboundService() *githuboutbound.Service {
	organization := tracker.OrganizationID("")
	if s.database != nil {
		organization = s.database.hostedOrganization
	}
	return githuboutbound.New(githuboutbound.Deps{
		Repository: s.config.TenantRepository, Organization: organization,
		Details: s.runDetailStore(), Backend: s.config.OutboxBackend, Now: s.config.now,
		Jitter: s.config.jitter, NewLeaseID: s.config.newLeaseID, Logger: s.config.Logger,
		OutboxPollInterval: s.config.OutboxPollInterval, OutboxProcessingTimeout: s.config.OutboxProcessingTimeout,
		OutboxBaseBackoff: s.config.OutboxBaseBackoff, OutboxMaxBackoff: s.config.OutboxMaxBackoff,
		Disabled: ErrOutboxDisabled, Unavailable: hubapi.ErrPostgresFeatureUnavailable,
		SplitRepository: splitRepositoryFullName, Process: s.ProcessOutbox,
		NewTimer: func(delay time.Duration) githuboutbound.Timer {
			timer := s.config.newOutboxTimer(delay)
			return githuboutbound.Timer{Ticks: timer.ticks, Reset: timer.reset, Stop: timer.stop}
		},
	})
}
func (s *Service) processGitHubOutboundRecord(ctx context.Context) (bool, error) {
	return s.githubOutboundService().Process(ctx)
}

type outboxTimer struct {
	ticks <-chan time.Time
	reset func(time.Duration)
	stop  func()
}
type outboxWorker struct{ worker *githuboutbound.Worker }

func newOutboxWorker(s *Service) *outboxWorker {
	return &outboxWorker{worker: githuboutbound.NewWorker(s.githubOutboundService())}
}
func (w *outboxWorker) start(ctx context.Context) { w.worker.Start(ctx) }
func (w *outboxWorker) stop() {
	if w != nil {
		w.worker.Stop()
	}
}
