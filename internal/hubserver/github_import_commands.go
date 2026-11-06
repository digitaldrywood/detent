package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) startGitHubImportRecord(ctx context.Context, scope nativeScope, operation string, request startGitHubImportRequest) (json.RawMessage, error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.authorizedChangeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash); found || err != nil {
		return raw, err
	}
	project, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	if project.Settings.Intake != "manual" || project.Settings.Repository == "" || request.IssueNumber <= 0 {
		return nil, nativePublicInvalid("Enable manual intake on a repository-backed project and provide a positive issue number")
	}
	var current projectstore.GitHubImport
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(projectstore.GitHubImportReader)
		if !ok {
			return errors.New("GitHub import owner unavailable")
		}
		var err error
		current, err = owner.GitHubImportForIssue(ctx, scope.project, int64(request.IssueNumber))
		return err
	})
	created := errors.Is(err, corestore.ErrNotFound)
	if err != nil && !created {
		return nil, projectRecordError(err)
	}
	next := current
	if created {
		identity, err := nativeCommandHash([]string{string(scope.organization), string(scope.project), scope.credential.ID, request.IdempotencyKey})
		if err != nil {
			return nil, err
		}
		next = projectstore.GitHubImport{OrganizationID: scope.organization, ProjectID: scope.project, ID: "imp_" + identity[:32], IssueNumber: int64(request.IssueNumber), Stage: "issue", Status: "pending", Revision: 1, Gaps: []string{}, ObservedAt: s.config.now()}
	} else if request.Restart {
		if current.Revision != int64(request.ExpectedRevision) {
			return nil, nativeConflict(tracker.Revision(current.Revision))
		}
		next.Stage, next.Cursor, next.Status, next.LastError = "issue", "", "pending", ""
		next.EditSequence, next.RetryAfter, next.Gaps = 0, nil, []string{}
		next.Revision++
	}
	return s.importRecordMutation(ctx, scope, operation, request.Mutation, request, importJobResponse(next), func(w tenantstore.Writer) error {
		if err := s.database.requireHostedFeatureRecord(ctx, w, "github_integration", s.config.now()); err != nil {
			return err
		}
		latest, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if latest.Settings.Revision != project.Settings.Revision {
			return nativeConflict(tracker.Revision(latest.Settings.Revision))
		}
		owner, ok := w.(projectstore.GitHubImportWriter)
		if !ok {
			return errors.New("GitHub import owner unavailable")
		}
		if created {
			_, err = owner.CreateGitHubImport(ctx, next)
		} else if request.Restart {
			next.Revision = current.Revision
			_, err = owner.UpdateGitHubImport(ctx, next)
		} else {
			actual, readErr := owner.GitHubImport(ctx, scope.project, current.ID)
			if readErr != nil {
				return readErr
			}
			if actual.Revision != current.Revision {
				return nativeConflict(tracker.Revision(actual.Revision))
			}
		}
		return err
	})
}

func (s *Service) importRecordMutation(ctx context.Context, scope nativeScope, operation string, command tracker.Mutation, input, value any, apply func(tenantstore.Writer) error) (json.RawMessage, error) {
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := nativeCommandHash(input)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.changeRecordReceipt(ctx, scope, operation, command.IdempotencyKey, hash); found || err != nil {
		return raw, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	now := s.config.now()
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, err
	}
	var existing *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		previous, err := w.Receipt(ctx, scope.credential.ID, operation, command.IdempotencyKey)
		if err == nil {
			if previous.RequestHash != hash {
				return nativeConflict(0)
			}
			existing = &previous
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := apply(w); err != nil {
			return err
		}
		if scope.credential.Hosted != nil {
			audit := projectRecordActor(scope).Audit
			audit.Event, audit.Route, audit.Status = "action", operation, http.StatusOK
			audit.RecordedAt = s.config.now()
			audit.RecordedAtNS = audit.RecordedAt.UnixNano()
			if _, err := w.RecordAccessAudit(ctx, audit); err != nil {
				return err
			}
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if errors.Is(err, corestore.ErrRevisionConflict) || errors.Is(err, corestore.ErrUniqueViolation) {
		return nil, nativeConflict(0)
	}
	if err != nil {
		return nil, projectRecordError(err)
	}
	if existing != nil {
		return content.Receipt(ctx, *existing)
	}
	return raw, nil
}
