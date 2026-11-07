package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) prepareGitHubSummaryRecord(ctx context.Context, scope nativeScope, issue tracker.NativeIssue) (*projectstore.GitHubOutbound, error) {
	if s.config.Hosted == nil || issue.LinkedSource == nil {
		return nil, nil
	}
	_, repository, number, err := tracker.ParseGitHubIssueURL(issue.LinkedSource.URL)
	if err != nil {
		return nil, err
	}
	path, err := s.readIssueBrowserPath(ctx, string(issue.WorkItemID))
	if err != nil {
		return nil, err
	}
	desired, err := json.Marshal(WorkpadDesired{Phase: "summary", Body: "Tracked in Detent: " + s.config.Hosted.PublicURL + path, Marker: "<!-- detent-summary:" + string(issue.WorkItemID) + ":intake -->", Summary: true, IssueNumber: number})
	if err != nil {
		return nil, err
	}
	payload, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization}, desired)
	if err != nil {
		return nil, err
	}
	return &projectstore.GitHubOutbound{ProjectID: scope.project, WorkItemID: issue.WorkItemID, Key: "intake:" + string(issue.WorkItemID), Repository: repository, IssueNumber: int64(number), Kind: string(MutationWorkpad), CreatedAt: s.config.now(), Payload: payload}, nil
}

func (s *Service) processGitHubOutboundRecord(ctx context.Context) (bool, error) {
	if s.config.OutboxBackend == nil || s.config.TenantRepository == nil {
		return false, ErrOutboxDisabled
	}
	organization := s.database.hostedOrganization
	var row projectstore.GitHubOutbound
	err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		var err error
		row, err = w.(projectstore.GitHubOutboxWriter).ClaimGitHubOutbound(ctx, s.config.now(), s.config.now().Add(s.config.OutboxProcessingTimeout), s.config.newLeaseID())
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
		return w.(projectstore.GitHubOutboxWriter).FinishGitHubOutbound(cleanup, row)
	})
	return err == nil && writeErr == nil, errors.Join(err, writeErr)
}
