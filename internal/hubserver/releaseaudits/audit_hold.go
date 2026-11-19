package releaseaudits

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AuditHoldClearance struct {
	Source                 tracker.NativeChangeReference
	WorkItem               tracker.NativeWorkItemID
	VersionCreatedAt       time.Time
	ReviewCreatedAt        time.Time
	PolicyID               string
	ReviewEvidenceRevision int64
	ReviewSequence         int64
	EvidenceSequence       int64
	HumanAction            string
}

type auditHoldEvidence struct {
	change       eventstore.Change
	version      eventstore.Version
	review       eventstore.Evidence
	policy       policystore.Revision
	dispositions []eventstore.AuditDisposition
}

func (s *ChangeEvidence) recoverClearedAuditHold(ctx context.Context, scope hubscope.NativeScope, receipt eventstore.AuditDisposition) error {
	facts, err := s.readAuditHoldEvidence(ctx, scope, receipt)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	content := eventstore.NewContent(s.deps.RunDetailStore())
	raw, err := content.Evidence(ctx, facts.review)
	if err != nil {
		return err
	}
	var review tracker.ChangeReview
	if err := json.Unmarshal(raw, &review); err != nil {
		return err
	}
	audit := review.SecurityAudit
	if audit == nil || audit.RunOmitted || review.VersionID != facts.version.ID || audit.VersionID != facts.version.ID ||
		audit.SessionID != receipt.Disposition.AuditRunID || audit.Run.ID != audit.SessionID || audit.Run.IssueID != string(facts.change.WorkItemID) {
		return nil
	}
	action := strings.TrimSpace(audit.Run.HumanAction())
	if action == "" {
		return nil
	}
	descriptor, err := content.ProjectPolicy(ctx, facts.policy)
	if err != nil {
		return err
	}
	workflow, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, descriptor)
	if err != nil {
		return err
	}
	dispositions := make([]securityaudit.Disposition, 0, len(facts.dispositions))
	for _, row := range facts.dispositions {
		if row.ChangeID != facts.change.ID || row.ReviewSequence != facts.review.Sequence {
			continue
		}
		value, err := s.readAuditDisposition(ctx, scope, row)
		if err != nil {
			return err
		}
		dispositions = append(dispositions, value)
	}
	key := securityaudit.Key{ProjectID: string(scope.Project), Repository: facts.version.Repository, PRNumber: audit.Run.PRNumber, BaseSHA: facts.version.BaseSHA, HeadSHA: facts.version.HeadSHA}
	if !securityaudit.Evaluate(audit.Run, dispositions, key, securityaudit.ServiceIdentity(string(scope.Project)), workflow.Config.Gate.SecurityAudit.BlockOn).Allowed {
		return nil
	}
	return s.deps.RecoverAuditHold(ctx, scope, AuditHoldClearance{
		Source:   tracker.NativeChangeReference{ChangeID: facts.change.ID, VersionID: facts.version.ID, HeadSHA: facts.version.HeadSHA},
		WorkItem: facts.change.WorkItemID, VersionCreatedAt: facts.version.CreatedAt, ReviewCreatedAt: facts.review.CreatedAt,
		PolicyID: facts.policy.PolicyID, ReviewEvidenceRevision: facts.change.ReviewEvidenceRevision, ReviewSequence: facts.review.Sequence, EvidenceSequence: facts.change.EvidenceSequence, HumanAction: action,
	})
}

func (s *ChangeEvidence) readAuditHoldEvidence(ctx context.Context, scope hubscope.NativeScope, receipt eventstore.AuditDisposition) (auditHoldEvidence, error) {
	var facts auditHoldEvidence
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.deps.AuthorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		change, err := r.Change(ctx, receipt.ChangeID)
		if err != nil {
			return err
		}
		if change.OrganizationID != scope.Organization || change.ProjectID != scope.Project || change.CurrentVersionID == nil {
			return corestore.ErrNotFound
		}
		item, err := r.WorkItem(ctx, scope.Project, change.WorkItemID)
		if err != nil {
			return err
		}
		if item.Archived || !strings.EqualFold(hubapi.StringValue(item.WorkflowStateName), "Blocked") {
			return corestore.ErrNotFound
		}
		latest, ok := r.(eventstore.LatestAuditReviewReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		sequence, err := latest.LatestAuditReviewSequence(ctx, change.ID, *change.CurrentVersionID)
		if err != nil {
			return err
		}
		if sequence != receipt.ReviewSequence {
			return corestore.ErrNotFound
		}
		rows, err := r.Evidence(ctx, change.ID, sequence-1, 1)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].Sequence != sequence || rows[0].Kind != "review" || hubapi.StringValue(rows[0].VersionID) != *change.CurrentVersionID {
			return corestore.ErrRevisionConflict
		}
		version, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
		if err != nil {
			return err
		}
		approved, err := r.ProjectPolicy(ctx, scope.Project)
		if err != nil {
			return err
		}
		if version.PolicyRevisionID != approved.PolicyID {
			return corestore.ErrNotFound
		}
		reader, ok := r.(eventstore.AuditDispositionReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		dispositions, err := reader.AuditDispositions(ctx, scope.Project, receipt.Disposition.AuditRunID)
		if err != nil {
			return err
		}
		facts = auditHoldEvidence{change: change, version: version, review: rows[0], policy: approved, dispositions: dispositions}
		return nil
	})
	return facts, err
}
