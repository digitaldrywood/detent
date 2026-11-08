package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) processGitHubOutboundRecord(ctx context.Context) (bool, error) {
	if s.config.OutboxBackend == nil || s.config.TenantRepository == nil {
		return false, ErrOutboxDisabled
	}
	organization := s.database.hostedOrganization
	var row projectstore.GitHubOutbound
	err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		var err error
		gitHubOutboxWriter, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		row, err = gitHubOutboxWriter.ClaimGitHubOutbound(ctx, s.config.now(), s.config.now().Add(s.config.OutboxProcessingTimeout), s.config.newLeaseID())
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	desired, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, row.Payload)
	if err == nil {
		owner, name, valid := splitRepositoryFullName(row.Repository)
		if !valid {
			return false, errors.New("GitHub outbound repository is invalid")
		}
		item := OutboxItem{ID: row.ID, IdempotencyKey: row.Key, Profile: "native", RepositoryOwner: owner, RepositoryName: name, IssueNumber: int(row.IssueNumber), Kind: MutationKind(row.Kind), TargetNodeID: row.TargetNodeID, Desired: desired, Status: row.Status, Attempts: int(row.Attempts), CreatedAt: row.CreatedAt}
		if item.Kind.irreversible() {
			err = s.config.OutboxBackend.VerifyAndExecute(ctx, item)
		} else {
			err = s.config.OutboxBackend.Execute(ctx, item)
		}
	}
	row.Status = "completed"
	if err != nil {
		row.Status = "retrying"
		delay := min(s.config.OutboxBaseBackoff*time.Duration(1<<min(row.Attempts-1, 20)), s.config.OutboxMaxBackoff)
		row.NextAttemptAt = s.config.now().Add(time.Duration(float64(delay) * (0.5 + s.config.jitter())))
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	writeErr := s.config.TenantRepository.WriteTenant(cleanup, organization, corestore.Background, func(w tenantstore.Writer) error {
		gitHubOutboxWriter, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		return gitHubOutboxWriter.FinishGitHubOutbound(cleanup, row)
	})
	return err == nil && writeErr == nil, errors.Join(err, writeErr)
}
