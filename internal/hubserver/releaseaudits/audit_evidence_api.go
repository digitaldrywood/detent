package releaseaudits

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *ChangeEvidence) authorizeAuditChange(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, changeID string) error {
	if scope.Credential.Runner.RunnerID == "" && scope.Credential.HostedRole != "reporter" {
		return nil
	}
	change, err := r.Change(ctx, changeID)
	if err != nil {
		return err
	}
	if change.OrganizationID != scope.Organization || change.ProjectID != scope.Project {
		return corestore.ErrNotFound
	}
	item, err := r.WorkItem(ctx, scope.Project, change.WorkItemID)
	if err != nil {
		return err
	}
	if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
		return corestore.ErrNotFound
	}
	if scope.Credential.Runner.RunnerID == "" {
		return nil
	}
	lease, err := r.ActiveLeaseForIssue(ctx, item.ID)
	if err != nil {
		return err
	}
	fence := runnersession.Mutation{LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken)}
	_, _, err = s.deps.RunnerSessionFence(ctx, r, scope, fence, string(change.WorkItemID), false)
	if errors.Is(err, tracker.ErrStaleFencingToken) {
		return corestore.ErrNotFound
	}
	return err
}

func (s *ChangeEvidence) visibleAuditCandidates(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, rows []eventstore.Evidence) ([]eventstore.Evidence, error) {
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

func (s *ChangeEvidence) auditDispositions(ctx context.Context, scope hubscope.NativeScope, id int64) ([]securityaudit.Disposition, error) {
	var rows []eventstore.AuditDisposition
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.deps.AuthorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(eventstore.AuditDispositionReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		rows, err = reader.AuditDispositions(ctx, scope.Project, id)
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

func (s *ChangeEvidence) GetAuditDispositions(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("session"), 10, 64)
	if err != nil || id < 1 {
		return s.deps.NativeAPIError(c, hubapi.NativeNotFound())
	}
	ctx := c.Request().Context()
	value, err := s.auditDispositions(ctx, hubscope.NativeRequestScope(c), id)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *ChangeEvidence) GetAuditEvidence(c echo.Context) error {
	if c.QueryParam("change_id") != "" || c.QueryParam("version_id") != "" || c.QueryParam("before_review_sequence") != "" {
		return s.getPriorAuditEvidence(c)
	}
	ctx := c.Request().Context()
	scope := hubscope.NativeRequestScope(c)
	pr, err := strconv.Atoi(c.QueryParam("pull_request"))
	if err != nil || pr < 0 || c.QueryParam("repository") == "" {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("repository and pull request are required"))
	}
	key := securityaudit.Key{Repository: c.QueryParam("repository"), PRNumber: pr, BaseSHA: c.QueryParam("base_sha"), HeadSHA: c.QueryParam("head_sha")}
	var candidates []eventstore.Evidence
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.deps.AuthorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(eventstore.AuditDispositionReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		candidates, err = reader.AuditReviewCandidates(ctx, scope.Project, key, 100)
		if err == nil {
			candidates, err = s.visibleAuditCandidates(ctx, r, scope, candidates)
		}
		return err
	})
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	for _, candidate := range candidates {
		raw, err := eventstore.NewContent(s.deps.RunDetailStore()).Evidence(ctx, candidate)
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		var review struct {
			SecurityAudit *struct{ Run securityaudit.Run } `json:"security_audit"`
		}
		if err := json.Unmarshal(raw, &review); err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		if review.SecurityAudit == nil || review.SecurityAudit.Run.ProjectID != string(scope.Project) || review.SecurityAudit.Run.Repository != key.Repository || review.SecurityAudit.Run.PRNumber != key.PRNumber || key.BaseSHA != "" && review.SecurityAudit.Run.BaseSHA != key.BaseSHA || key.HeadSHA != "" && review.SecurityAudit.Run.HeadSHA != key.HeadSHA {
			continue
		}
		run := review.SecurityAudit.Run
		dispositions, err := s.auditDispositions(ctx, scope, run.ID)
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, securityaudit.Evidence{Run: run, Dispositions: dispositions})
	}
	return s.deps.NativeAPIError(c, hubapi.NativeNotFound())
}

type priorAuditEvidence struct {
	VersionID                string                      `json:"version_id"`
	VersionNumber            int64                       `json:"version_number"`
	ReviewSequence           int64                       `json:"review_sequence,omitempty"`
	Landing                  bool                        `json:"landing,omitempty"`
	Run                      *securityaudit.Run          `json:"run,omitempty"`
	Dispositions             []securityaudit.Disposition `json:"dispositions"`
	NextBeforeReviewSequence int64                       `json:"next_before_review_sequence,omitempty"`
}

func (s *ChangeEvidence) getPriorAuditEvidence(c echo.Context) error {
	ctx := c.Request().Context()
	scope := hubscope.NativeRequestScope(c)
	query := eventstore.PriorAuditQuery{
		Project: scope.Project, ChangeID: c.QueryParam("change_id"), VersionID: c.QueryParam("version_id"),
		Repository: c.QueryParam("repository"), ThroughReviewSequence: math.MaxInt64,
	}
	if query.ChangeID == "" || query.VersionID == "" || query.Repository == "" {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("change, version and repository are required"))
	}
	if cursor := c.QueryParam("before_review_sequence"); cursor != "" {
		before, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || before < 1 {
			return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("invalid review cursor"))
		}
		query.ThroughReviewSequence = before - 1
	}
	var snapshot eventstore.PriorAuditSnapshot
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.deps.AuthorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		if err := s.authorizeAuditChange(ctx, r, scope, query.ChangeID); err != nil {
			return err
		}
		reader, ok := r.(eventstore.PriorAuditReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		snapshot, err = reader.PriorAuditEvidence(ctx, query)
		return err
	})
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	value, err := s.readPriorAuditEvidence(ctx, scope, snapshot)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *ChangeEvidence) readPriorAuditEvidence(ctx context.Context, scope hubscope.NativeScope, snapshot eventstore.PriorAuditSnapshot) (priorAuditEvidence, error) {
	version, review := snapshot.Version, snapshot.Review
	value := priorAuditEvidence{VersionID: version.ID, VersionNumber: version.Number, ReviewSequence: review.Sequence, NextBeforeReviewSequence: snapshot.NextBeforeReviewSequence, Dispositions: []securityaudit.Disposition{}}
	if review.Sequence == 0 {
		return value, nil
	}
	raw, err := eventstore.NewContent(s.deps.RunDetailStore()).Evidence(ctx, review)
	if err != nil {
		return value, err
	}
	var decoded tracker.ChangeReview
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return value, err
	}
	audit := decoded.SecurityAudit
	if audit == nil || audit.Run.ID < 1 || audit.RunOmitted {
		return value, nil
	}
	run := audit.Run
	if decoded.VersionID != version.ID || audit.SessionID != run.ID ||
		run.ProjectID != string(scope.Project) || run.Repository != version.Repository || !priorAuditSourceMatches(version, *audit) {
		return value, corestore.ErrRevisionConflict
	}
	value.Run, value.Landing = &run, audit.Landing
	findings := make(map[string]struct{}, len(run.Findings))
	for _, finding := range run.Findings {
		findings[finding.ID] = struct{}{}
	}
	for _, receipt := range snapshot.Dispositions {
		d := receipt.Disposition
		if _, found := findings[d.FindingID]; !found || d.AuditRunID != run.ID {
			continue
		}
		entry, err := s.readAuditDisposition(ctx, scope, receipt)
		if err != nil {
			return value, err
		}
		value.Dispositions = append(value.Dispositions, entry)
	}
	return value, nil
}

func priorAuditSourceMatches(version eventstore.Version, audit tracker.SecurityAuditReview) bool {
	if audit.Landing {
		return audit.VersionID == version.ID && audit.Run.BaseSHA != audit.Run.HeadSHA &&
			changerequest.ValidHash(audit.Run.BaseSHA, len(version.BaseSHA)) && changerequest.ValidHash(audit.Run.HeadSHA, len(version.HeadSHA))
	}
	return (audit.VersionID == "" || audit.VersionID == version.ID) &&
		audit.Run.BaseSHA == version.BaseSHA && audit.Run.HeadSHA == version.HeadSHA
}

func (s *ChangeEvidence) readAuditDisposition(ctx context.Context, scope hubscope.NativeScope, row eventstore.AuditDisposition) (securityaudit.Disposition, error) {
	var value securityaudit.Disposition
	raw, err := s.deps.RunDetailStore().ReadDetail(ctx, string(scope.Organization), row.Reference)
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, err
	}
	if value.AuditRunID != row.Disposition.AuditRunID || value.FindingID != row.Disposition.FindingID || value.Status != row.Disposition.Status || value.ServiceIdentity != securityaudit.ServiceIdentity(string(scope.Project)) {
		return value, corestore.ErrRevisionConflict
	}
	value.ID = row.Disposition.ID
	return value, nil
}

func (s *ChangeEvidence) PrepareValidatorAuditDispositions(ctx context.Context, scope hubscope.NativeScope, changeID string, run securityaudit.Run) ([]securityaudit.Disposition, []eventstore.AuditDisposition, error) {
	return s.prepareValidatorAuditDispositionsFor(ctx, scope, changeID, run, corestore.Serving, nil)
}

func (s *ChangeEvidence) prepareValidatorAuditDispositionsFor(ctx context.Context, scope hubscope.NativeScope, changeID string, run securityaudit.Run, workload corestore.Workload, facts *tenantstore.BoardChangeRows) ([]securityaudit.Disposition, []eventstore.AuditDisposition, error) {
	var rows []eventstore.AuditDisposition
	var sources []eventstore.Evidence
	var err error
	if facts != nil && facts.AuditDispositions != nil {
		var covered bool
		rows, covered = facts.AuditDispositions[run.ID]
		if !covered {
			return nil, nil, corestore.ErrRevisionConflict
		}
		if len(rows) > 200 {
			return nil, nil, corestore.ErrInvalidPage
		}
		for _, row := range rows {
			if row.Organization != scope.Organization || row.Project != scope.Project || row.ChangeID != changeID {
				return nil, nil, corestore.ErrRevisionConflict
			}
			found := false
			for _, source := range facts.Evidence {
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
		err = s.deps.Repository.ReadTenant(ctx, scope.Organization, workload, func(r tenantstore.Reader) error {
			if workload != corestore.Background {
				if err := s.deps.AuthorizeAccessScope(ctx, r, scope, false); err != nil {
					return err
				}
			}
			reader, ok := r.(eventstore.AuditDispositionReader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			var err error
			rows, err = reader.AuditDispositions(ctx, scope.Project, run.ID)
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
		raw, err := eventstore.NewContent(s.deps.RunDetailStore()).Evidence(ctx, sources[i])
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
		if source.ID != run.ID || source.ProjectID != string(scope.Project) || source.IssueID != run.IssueID || source.Repository != run.Repository || source.PRNumber != run.PRNumber || source.BaseSHA != run.BaseSHA || source.HeadSHA != run.HeadSHA || !reflect.DeepEqual(source.Findings, run.Findings) {
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

func LockValidatorAuditDispositions(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, runID int64, expected []eventstore.AuditDisposition) error {
	reader, ok := r.(eventstore.AuditDispositionReader)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	rows, err := reader.AuditDispositions(ctx, scope.Project, runID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(rows, expected) {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *ChangeEvidence) SummarizeChangeWithAuditDispositions(ctx context.Context, scope hubscope.NativeScope, detail tracker.ChangeDetail, policyID, reviewPolicyID string, now time.Time, priorApproval bool) (tracker.ChangeSummary, error) {
	return s.SummarizeChangeWithAuditDispositionsFor(ctx, scope, detail, policyID, reviewPolicyID, now, priorApproval, corestore.Serving, nil)
}

func (s *ChangeEvidence) SummarizeChangeWithAuditDispositionsFor(ctx context.Context, scope hubscope.NativeScope, detail tracker.ChangeDetail, policyID, reviewPolicyID string, now time.Time, priorApproval bool, workload corestore.Workload, facts *tenantstore.BoardChangeRows) (tracker.ChangeSummary, error) {
	dispositions := make(map[int64][]securityaudit.Disposition)
	for _, review := range detail.Reviews {
		if review.VersionID != detail.Change.CurrentVersion || review.SecurityAudit == nil {
			continue
		}
		run := review.SecurityAudit.Run
		if _, exists := dispositions[run.ID]; exists {
			continue
		}
		values, _, err := s.prepareValidatorAuditDispositionsFor(ctx, scope, detail.Change.ID, run, workload, facts)
		if err != nil {
			return tracker.ChangeSummary{}, err
		}
		dispositions[run.ID] = values
	}
	return changerequest.SummarizeCurrentVersionWithAuditDispositions(detail, policyID, reviewPolicyID, now, priorApproval, dispositions), nil
}
