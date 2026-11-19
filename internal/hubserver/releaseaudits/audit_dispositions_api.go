package releaseaudits

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func (s *ChangeEvidence) PostAuditDisposition(c echo.Context) error {
	var request securityaudit.DispositionCommand
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.deps.InvalidAPIRequest(c, err)
	}
	if request.RequestID == "" || len(request.RequestID) > 128 || !request.Confirm || strings.TrimSpace(request.FindingID) == "" || strings.TrimSpace(request.Evidence) == "" || len(request.Evidence) > securityaudit.MaxDispositionEvidenceBytes || request.Key.Repository == "" || request.Key.BaseSHA == "" || request.Key.HeadSHA == "" {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("confirmed disposition requires exact source, finding and bounded evidence"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope := hubscope.NativeRequestScope(c)
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	var receipt *eventstore.AuditDisposition
	var candidates []eventstore.Evidence
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.deps.AuthorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		reader, ok := r.(eventstore.AuditDispositionReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		prior, err := reader.AuditDispositionRequest(ctx, scope.Project, scope.Credential.ID, request.RequestID)
		if err == nil {
			if err := s.authorizeAuditChange(ctx, r, scope, prior.ChangeID); err != nil {
				return err
			}
			if prior.RequestHash != hash {
				return &hubapi.NativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, PublicMessage: true}
			}
			receipt = &prior
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		candidates, err = reader.AuditReviewCandidates(ctx, scope.Project, request.Key, 100)
		if err == nil {
			candidates, err = s.visibleAuditCandidates(ctx, r, scope, candidates)
		}
		return err
	})
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	if receipt != nil {
		return s.respondAuditDisposition(c, ctx, scope, *receipt)
	}
	var selected *eventstore.Evidence
	var run securityaudit.Run
	for _, candidate := range candidates {
		raw, err := eventstore.NewContent(s.deps.RunDetailStore()).Evidence(ctx, candidate)
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		var review tracker.ChangeReview
		if err := json.Unmarshal(raw, &review); err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		if review.SecurityAudit == nil || candidate.VersionID == nil || review.SecurityAudit.VersionID != *candidate.VersionID || review.SecurityAudit.SessionID != review.SecurityAudit.Run.ID {
			continue
		}
		audit := review.SecurityAudit.Run
		if audit.ProjectID != string(scope.Project) || (request.Key.ProjectID != "" && request.Key.ProjectID != string(scope.Project)) || audit.Repository != request.Key.Repository || audit.BaseSHA != request.Key.BaseSHA || audit.HeadSHA != request.Key.HeadSHA || audit.PRNumber != request.Key.PRNumber {
			continue
		}
		for _, finding := range audit.Findings {
			if finding.ID == request.FindingID {
				selected = &candidate
				run = audit
				break
			}
		}
		break
	}
	if selected == nil || run.ID < 1 {
		return s.deps.NativeAPIError(c, hubapi.NativeNotFound())
	}
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.authorizeAuditDisposition(ctx, r, scope, *selected, run, false)
	})
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	disposition := securityaudit.Disposition{AuditRunID: run.ID, FindingID: request.FindingID, Status: securityaudit.DispositionFalsePositive, Evidence: strings.TrimSpace(request.Evidence), ServiceIdentity: securityaudit.ServiceIdentity(string(scope.Project)), RecordedAt: s.deps.Clock().UTC()}
	raw, err := json.Marshal(disposition)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	ref, err := s.deps.RunDetailStore().StoreDetail(ctx, string(scope.Organization), raw)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	if err := s.deps.ValidateNativeDetailReference(ctx, s.deps.RunDetailStore(), scope.Organization, ref, raw); err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	row := eventstore.AuditDisposition{ActorID: scope.Credential.ID, Organization: scope.Organization, Project: scope.Project, ChangeID: selected.ChangeID, ReviewSequence: selected.Sequence, Disposition: disposition, Reference: ref}
	row.RequestKey = request.RequestID
	row.RequestHash = hash
	err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.deps.AuthorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		if err := s.authorizeAuditDisposition(ctx, w, scope, *selected, run, true); err != nil {
			return err
		}
		writer, ok := w.(eventstore.AuditDispositionWriter)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		row, err = writer.CreateAuditDisposition(ctx, row)
		return err
	})
	if err != nil {
		if errors.Is(err, corestore.ErrRevisionConflict) {
			err = hubapi.NativeConflict(0)
		}
		return s.deps.NativeAPIError(c, err)
	}
	return s.respondAuditDisposition(c, ctx, scope, row)
}

func (s *ChangeEvidence) respondAuditDisposition(c echo.Context, ctx context.Context, scope hubscope.NativeScope, row eventstore.AuditDisposition) error {
	value, err := s.readAuditDisposition(ctx, scope, row)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	if err := s.recoverClearedAuditHold(ctx, scope, row); err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *ChangeEvidence) authorizeAuditDisposition(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, evidence eventstore.Evidence, run securityaudit.Run, lock bool) error {
	if err := s.deps.AuthorizeAccessScope(ctx, r, scope, true); err != nil {
		return err
	}
	change, err := r.Change(ctx, evidence.ChangeID)
	if err != nil {
		return err
	}
	if change.ProjectID != scope.Project || evidence.VersionID == nil {
		return hubapi.NativeNotFound()
	}
	version, err := r.Version(ctx, change.ID, *evidence.VersionID)
	if err != nil {
		return err
	}
	approved, err := r.ProjectPolicy(ctx, scope.Project)
	if err != nil {
		return err
	}
	if version.PolicyRevisionID != approved.PolicyID || version.Repository != run.Repository || version.BaseSHA != run.BaseSHA || version.HeadSHA != run.HeadSHA {
		return tracker.ErrStaleFencingToken
	}
	if scope.Credential.Runner.RunnerID == "" {
		if scope.Credential.Scope != hubapi.APIScopeAdmin && scope.Credential.HostedRole != "owner" && scope.Credential.HostedRole != "admin" {
			return accessstore.ErrDenied
		}
		scope.RequireHostedAdmin = true
		return s.deps.AuthorizeAccessScope(ctx, r, scope, true)
	}
	reader, ok := r.(runnersession.Reader)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	session, err := reader.RunnerSession(ctx, scope.Project, run.ID)
	if err != nil {
		return err
	}
	if session.Role != "security_audit" || session.IssueID != string(change.WorkItemID) || session.PolicyID != approved.PolicyID {
		return accessstore.ErrDenied
	}
	attempt, err := r.Attempt(ctx, session.AttemptID)
	if err != nil {
		return err
	}
	current, _, err := s.deps.RunnerSessionFence(ctx, r, scope, runnersession.Mutation{LeaseID: tracker.LeaseID(attempt.LeaseID), FencingToken: tracker.FencingToken(attempt.FencingToken)}, session.IssueID, lock)
	if err != nil {
		return err
	}
	if current.ID != session.AttemptID {
		return tracker.ErrStaleFencingToken
	}
	return nil
}
