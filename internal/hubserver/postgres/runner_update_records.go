package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/runnerrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func (s *session) runnerRecords() *runnerrecords.Store {
	return runnerrecords.New(runnerrecords.Deps{Organization: s.organization, DB: s.db, Identity: s.RunnerIdentity, CheckOrganization: s.checkOrganization, MapError: mapError, UpdateOwned: updateOwned})
}
func (s *session) RunnerUpdateRequest(ctx context.Context, id string) (runnerstore.UpdateRequestRecord, error) {
	return s.runnerRecords().RunnerUpdateRequest(ctx, id)
}
func (s *session) RunnerUpdateState(ctx context.Context, id string) (runnerstore.UpdateState, error) {
	return s.runnerRecords().RunnerUpdateState(ctx, id)
}
func (s *session) LockRunnerUpdate(ctx context.Context, id string) error {
	return s.runnerRecords().LockRunnerUpdate(ctx, id)
}
func (s *session) RunnerUrgentUpdate(ctx context.Context) (runnerstore.UrgentUpdate, error) {
	return s.runnerRecords().RunnerUrgentUpdate(ctx)
}
func (s *session) CreateRunnerUpdateRequest(ctx context.Context, r runnerstore.UpdateRequestRecord) error {
	return s.runnerRecords().CreateRunnerUpdateRequest(ctx, r)
}
func (s *session) EnsureAutomaticRunnerOffer(ctx context.Context, r runnerstore.UpdateRequestRecord, policy runnerstore.AutomaticOfferPolicy) (string, error) {
	return s.runnerRecords().EnsureAutomaticRunnerOffer(ctx, r, policy)
}
func (s *session) InvalidateAutomaticRunnerOffer(ctx context.Context, id, requestID string) error {
	return s.runnerRecords().InvalidateAutomaticRunnerOffer(ctx, id, requestID)
}
func (s *session) SetRunnerUpdateDesired(ctx context.Context, id string, revision int64, request string) error {
	return s.runnerRecords().SetRunnerUpdateDesired(ctx, id, revision, request)
}
func (s *session) ReplaceRunnerUpdateObservation(ctx context.Context, id string, observation *runnerauth.UpdateObservation) error {
	return s.runnerRecords().ReplaceRunnerUpdateObservation(ctx, id, observation)
}
func (s *session) SetRunnerUrgentUpdate(ctx context.Context, revision int64, request string, now time.Time) error {
	return s.runnerRecords().SetRunnerUrgentUpdate(ctx, revision, request, now)
}
func (s *session) RunnerLifecycle(ctx context.Context, id string) (*runnerauth.Lifecycle, error) {
	return s.runnerRecords().RunnerLifecycle(ctx, id)
}
func (s *session) RunnerLifecycleHistory(ctx context.Context, id string, limit int) ([]runnerstore.LifecycleTransition, error) {
	return s.runnerRecords().RunnerLifecycleHistory(ctx, id, limit)
}
func (s *session) RunnerLifecycleReceipt(ctx context.Context, id string, r runnerauth.LifecycleReport, digest string) (runnerstore.LifecycleRefusal, bool, error) {
	return s.runnerRecords().RunnerLifecycleReceipt(ctx, id, r, digest)
}
func (s *session) RefuseRunnerLifecycle(ctx context.Context, id string, r runnerauth.LifecycleReport, digest string, refusal runnerstore.LifecycleRefusal, now time.Time) error {
	return s.runnerRecords().RefuseRunnerLifecycle(ctx, id, r, digest, refusal, now)
}
func (s *session) ReportRunnerLifecycle(ctx context.Context, id string, r runnerauth.LifecycleReport, previousGeneration int64, digest string, rollout runnerstore.Rollout, now time.Time) error {
	return s.runnerRecords().ReportRunnerLifecycle(ctx, id, r, previousGeneration, digest, rollout, now)
}
func (s *session) RunnerStartDeadlines(ctx context.Context, now time.Time, limit int) ([]string, error) {
	return s.runnerRecords().RunnerStartDeadlines(ctx, now, limit)
}
func (s *session) ExpireRunnerStart(ctx context.Context, id string, now time.Time) error {
	return s.runnerRecords().ExpireRunnerStart(ctx, id, now)
}
func (s *session) RunnerUpdateTarget(ctx context.Context, id string) (runnerstore.UpdateTarget, error) {
	return s.runnerRecords().RunnerUpdateTarget(ctx, id)
}
func (s *session) SetRunnerUpdateTarget(ctx context.Context, id string, target runnerstore.UpdateTarget) error {
	return s.runnerRecords().SetRunnerUpdateTarget(ctx, id, target)
}
func (s *session) RunnerVersionFailed(ctx context.Context, version string) (bool, error) {
	return s.runnerRecords().RunnerVersionFailed(ctx, version)
}

func (s *session) LockOrganizationUpdateTarget(ctx context.Context, exclusive bool) error {
	return s.runnerRecords().LockOrganizationUpdateTarget(ctx, exclusive)
}
