package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/changerequest"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) authorizeAuditChange(ctx context.Context, r tenantstore.Reader, scope nativeScope, changeID string) error {
	if scope.credential.HostedRole != "reporter" {
		return nil
	}
	change, err := r.Change(ctx, changeID)
	if err != nil {
		return err
	}
	if change.ProjectID != scope.project {
		return corestore.ErrNotFound
	}
	item, err := r.WorkItem(ctx, scope.project, change.WorkItemID)
	if err != nil {
		return err
	}
	if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
		return corestore.ErrNotFound
	}
	return nil
}

func (s *Service) visibleAuditCandidates(ctx context.Context, r tenantstore.Reader, scope nativeScope, rows []eventstore.Evidence) ([]eventstore.Evidence, error) {
	result := make([]eventstore.Evidence, 0, len(rows))
	for _, row := range rows {
		if err := s.authorizeAuditChange(ctx, r, scope, row.ChangeID); err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			return nil, err
		}
		result = append(result, row)
	}
	return result, nil
}

func (s *Service) auditDispositions(ctx context.Context, scope nativeScope, id int64) ([]securityaudit.Disposition, error) {
	var rows []eventstore.AuditDisposition
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(eventstore.AuditDispositionReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var err error
		rows, err = reader.AuditDispositions(ctx, scope.project, id)
		if err == nil {
			for _, row := range rows {
				if err := s.authorizeAuditChange(ctx, r, scope, row.ChangeID); err != nil {
					return err
				}
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	result := make([]securityaudit.Disposition, 0, len(rows))
	for _, row := range rows {
		value, err := s.readAuditDisposition(ctx, scope, row)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

func (s *Service) getAuditDispositions(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("session"), 10, 64)
	if err != nil || id < 1 {
		return s.nativeAPIError(c, nativeNotFound())
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	value, err := s.auditDispositions(ctx, nativeRequestScope(c), id)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) getAuditEvidence(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	pr, err := strconv.Atoi(c.QueryParam("pull_request"))
	if err != nil || pr < 0 || c.QueryParam("repository") == "" {
		return s.nativeAPIError(c, nativePublicInvalid("repository and pull request are required"))
	}
	key := securityaudit.Key{Repository: c.QueryParam("repository"), PRNumber: pr, BaseSHA: c.QueryParam("base_sha"), HeadSHA: c.QueryParam("head_sha")}
	var candidates []eventstore.Evidence
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(eventstore.AuditDispositionReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var err error
		candidates, err = reader.AuditReviewCandidates(ctx, scope.project, key, 100)
		if err == nil {
			candidates, err = s.visibleAuditCandidates(ctx, r, scope, candidates)
		}
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	for _, candidate := range candidates {
		raw, err := eventstore.NewContent(s.runDetailStore()).Evidence(ctx, candidate)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		var review struct {
			SecurityAudit *struct{ Run securityaudit.Run } `json:"security_audit"`
		}
		if err := json.Unmarshal(raw, &review); err != nil {
			return s.nativeAPIError(c, err)
		}
		if review.SecurityAudit == nil || review.SecurityAudit.Run.ProjectID != string(scope.project) || review.SecurityAudit.Run.Repository != key.Repository || review.SecurityAudit.Run.PRNumber != key.PRNumber || key.BaseSHA != "" && review.SecurityAudit.Run.BaseSHA != key.BaseSHA || key.HeadSHA != "" && review.SecurityAudit.Run.HeadSHA != key.HeadSHA {
			continue
		}
		run := review.SecurityAudit.Run
		dispositions, err := s.auditDispositions(ctx, scope, run.ID)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, securityaudit.Evidence{Run: run, Dispositions: dispositions})
	}
	return s.nativeAPIError(c, nativeNotFound())
}

func (s *Service) readAuditDisposition(ctx context.Context, scope nativeScope, row eventstore.AuditDisposition) (securityaudit.Disposition, error) {
	var value securityaudit.Disposition
	raw, err := s.runDetailStore().ReadDetail(ctx, string(scope.organization), row.Reference)
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, err
	}
	if value.AuditRunID != row.Disposition.AuditRunID || value.FindingID != row.Disposition.FindingID || value.Status != row.Disposition.Status || value.ServiceIdentity != securityaudit.ServiceIdentity(string(scope.project)) {
		return value, corestore.ErrRevisionConflict
	}
	value.ID = row.Disposition.ID
	return value, nil
}

func (s *Service) prepareValidatorAuditDispositions(ctx context.Context, scope nativeScope, changeID string, run securityaudit.Run, facts ...*tenantstore.BoardChangeRows) ([]securityaudit.Disposition, []eventstore.AuditDisposition, error) {
	return s.prepareValidatorAuditDispositionsFor(ctx, scope, changeID, run, corestore.Serving, facts...)
}

func (s *Service) prepareValidatorAuditDispositionsFor(ctx context.Context, scope nativeScope, changeID string, run securityaudit.Run, workload corestore.Workload, facts ...*tenantstore.BoardChangeRows) ([]securityaudit.Disposition, []eventstore.AuditDisposition, error) {
	var rows []eventstore.AuditDisposition
	var sources []eventstore.Evidence
	var err error
	if len(facts) > 0 && facts[0] != nil && facts[0].AuditDispositions != nil {
		rows = facts[0].AuditDispositions[run.ID]
		if len(rows) > 200 {
			return nil, nil, corestore.ErrInvalidPage
		}
		for _, row := range rows {
			if row.Organization != scope.organization || row.Project != scope.project || row.ChangeID != changeID {
				return nil, nil, corestore.ErrRevisionConflict
			}
			found := false
			for _, source := range facts[0].Evidence {
				if source.Sequence == row.ReviewSequence && source.Kind == "review" {
					sources = append(sources, source)
					found = true
					break
				}
			}
			if !found {
				return nil, nil, corestore.ErrRevisionConflict
			}
		}
	} else {
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, workload, func(r tenantstore.Reader) error {
			if workload != corestore.Background {
				if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
					return err
				}
			}
			reader, ok := r.(eventstore.AuditDispositionReader)
			if !ok {
				return nativeRunEventStorageUnavailable()
			}
			var err error
			rows, err = reader.AuditDispositions(ctx, scope.project, run.ID)
			if err != nil {
				return err
			}
			for _, row := range rows {
				if row.ChangeID != changeID {
					return corestore.ErrRevisionConflict
				}
				if err := s.authorizeAuditChange(ctx, r, scope, row.ChangeID); err != nil {
					return err
				}
				evidence, err := r.Evidence(ctx, row.ChangeID, row.ReviewSequence-1, 1)
				if err != nil {
					return err
				}
				if len(evidence) != 1 || evidence[0].Sequence != row.ReviewSequence || evidence[0].Kind != "review" {
					return corestore.ErrRevisionConflict
				}
				sources = append(sources, evidence[0])
			}
			return nil
		})
	}
	if err != nil {
		return nil, nil, err
	}
	var values []securityaudit.Disposition
	for i, row := range rows {
		raw, err := eventstore.NewContent(s.runDetailStore()).Evidence(ctx, sources[i])
		if err != nil {
			return nil, nil, err
		}
		var review tracker.ChangeReview
		if err := json.Unmarshal(raw, &review); err != nil {
			return nil, nil, err
		}
		if review.SecurityAudit == nil {
			return nil, nil, corestore.ErrRevisionConflict
		}
		source := review.SecurityAudit.Run
		if source.ID != run.ID || source.ProjectID != string(scope.project) || source.IssueID != run.IssueID || source.Repository != run.Repository || source.PRNumber != run.PRNumber || source.BaseSHA != run.BaseSHA || source.HeadSHA != run.HeadSHA || !reflect.DeepEqual(source.Findings, run.Findings) {
			return nil, nil, corestore.ErrRevisionConflict
		}
		value, err := s.readAuditDisposition(ctx, scope, row)
		if err != nil {
			return nil, nil, err
		}
		values = append(values, value)
	}
	return values, rows, nil
}

func lockValidatorAuditDispositions(ctx context.Context, r tenantstore.Reader, scope nativeScope, runID int64, expected []eventstore.AuditDisposition) error {
	reader, ok := r.(eventstore.AuditDispositionReader)
	if !ok {
		return nativeRunEventStorageUnavailable()
	}
	rows, err := reader.AuditDispositions(ctx, scope.project, runID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(rows, expected) {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *Service) summarizeChangeWithAuditDispositions(ctx context.Context, scope nativeScope, detail tracker.ChangeDetail, policyID, reviewPolicyID string, now time.Time, priorApproval bool, facts ...*tenantstore.BoardChangeRows) (tracker.ChangeSummary, error) {
	return s.summarizeChangeWithAuditDispositionsFor(ctx, scope, detail, policyID, reviewPolicyID, now, priorApproval, corestore.Serving, facts...)
}

func (s *Service) summarizeChangeWithAuditDispositionsFor(ctx context.Context, scope nativeScope, detail tracker.ChangeDetail, policyID, reviewPolicyID string, now time.Time, priorApproval bool, workload corestore.Workload, facts ...*tenantstore.BoardChangeRows) (tracker.ChangeSummary, error) {
	dispositions := make(map[int64][]securityaudit.Disposition)
	for _, review := range detail.Reviews {
		if review.VersionID != detail.Change.CurrentVersion || review.SecurityAudit == nil {
			continue
		}
		run := review.SecurityAudit.Run
		if _, exists := dispositions[run.ID]; exists {
			continue
		}
		values, _, err := s.prepareValidatorAuditDispositionsFor(ctx, scope, detail.Change.ID, run, workload, facts...)
		if err != nil {
			return tracker.ChangeSummary{}, err
		}
		dispositions[run.ID] = values
	}
	return changerequest.SummarizeCurrentVersionWithAuditDispositions(detail, policyID, reviewPolicyID, now, priorApproval, dispositions), nil
}
