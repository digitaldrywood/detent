package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func enqueueHealthIssueRecord(ctx context.Context, writer postgres.HealthWriter, organization tracker.OrganizationID, row postgres.HealthRecord, finding healthFinding, at time.Time) error {
	if row.Class != "instance" {
		return nil
	}
	owner, ok := writer.(healthstore.IssueWriter)
	if !ok {
		return workItemStorageUnavailable()
	}
	digest, err := healthIssueDigest(finding)
	if err != nil {
		return err
	}
	if row.ResolvedAt != nil {
		sum := sha256.Sum256([]byte(digest + "\x00" + formatHubTime(*row.ResolvedAt)))
		digest = hex.EncodeToString(sum[:])
	}
	links, err := writer.HealthRecordProjects(ctx, postgres.HealthRecordProjectsInput{OrganizationID: organization, FindingID: row.ID, Limit: healthProjectLimit + 1})
	if err != nil {
		return err
	}
	if len(links) > healthProjectLimit {
		return errHealthReadLimit
	}
	for _, link := range links {
		if err := owner.EnqueueHealthIssueWork(ctx, healthstore.IssueWork{OrganizationID: organization, FindingID: row.ID, ProjectID: link.ProjectID, SourceRevision: row.Revision, SourceDigest: digest, NextCheckAt: at}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) maintainHealthIssue(ctx context.Context, organization tracker.OrganizationID, now time.Time) (bool, error) {
	var work healthstore.IssueWork
	var row postgres.HealthRecord
	var reports []healthstore.IssueReport
	err := s.database.postgres.ReadHealth(ctx, organization, corestore.Background, func(r postgres.HealthReader) error {
		owner, ok := r.(healthstore.IssueReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		work, err = owner.NextHealthIssueWork(ctx, now)
		if err != nil {
			return err
		}
		row, err = r.HealthRecord(ctx, postgres.HealthRecordInput{OrganizationID: organization, ID: work.FindingID})
		if err != nil {
			return err
		}
		if row.Revision != work.SourceRevision {
			return corestore.ErrRevisionConflict
		}
		if row.ResolvedAt != nil {
			reports, err = owner.HealthIssueReports(ctx, work.FindingID, work.ProjectID, work.AfterFingerprint, healthEvidenceLimit+1)
		}
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	moreReports := len(reports) > healthEvidenceLimit
	if moreReports {
		reports = reports[:healthEvidenceLimit]
	}
	if err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		owner, ok := w.(healthstore.IssueWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		work, err = owner.AdvanceHealthIssueWork(ctx, work, now)
		return err
	}); err != nil {
		return false, err
	}
	finding := healthFinding{ID: row.ID, Fingerprint: row.Fingerprint, Signal: row.Signal, Class: row.Class, Subject: healthSubject{Kind: row.SubjectKind, ID: row.SubjectID}, OpenedAt: row.OpenedAt, LastSeenAt: row.LastSeenAt, ResolvedAt: row.ResolvedAt, Severity: row.Severity, Summary: row.Summary, NextAction: row.NextAction}
	if row.EvidenceObjectKey != "" {
		raw, err := s.runDetailStore().ReadDetail(ctx, string(organization), eventstore.Reference(row.EvidenceObjectKey, row.EvidenceSha256, row.EvidenceBytes, row.EvidenceContentBytes))
		if err != nil {
			return false, err
		}
		if err := json.Unmarshal(raw, &finding.Evidence); err != nil {
			return false, err
		}
	}
	scope := healthIssueScope(organization, work.ProjectID)
	scope.credential = apiCredential{ID: "health_detector", Scope: apiScopeAdmin}
	scope.healthPublication = &healthIssuePublication{work: work, openedAt: finding.OpenedAt}
	if row.ResolvedAt == nil {
		if len(finding.Evidence.Signatures) > healthEvidenceLimit {
			return false, errHealthReadLimit
		}
		for _, occurrence := range healthIssueFindings(finding) {
			if err := s.publishHealthIssueOccurrence(ctx, scope, occurrence); err != nil {
				return false, err
			}
		}
	} else {
		for _, report := range reports {
			if err := s.resolveHealthIssueReport(ctx, scope, finding, report, now); err != nil {
				return false, err
			}
		}
	}
	pending := row.ResolvedAt == nil || !now.After(row.ResolvedAt.Add(healthReopenWindow))
	next := now.Add(healthInterval)
	if row.ResolvedAt != nil && pending {
		next = row.ResolvedAt.Add(healthReopenWindow + time.Microsecond)
	}
	work.AfterFingerprint = ""
	if moreReports {
		pending = true
		next = now
		work.AfterFingerprint = reports[len(reports)-1].IntakeFingerprint
	}
	err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		owner, ok := w.(healthstore.IssueWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		return owner.FinishHealthIssueWork(ctx, work, pending, next)
	})
	return err == nil, err
}

func (s *Service) publishHealthIssueOccurrence(ctx context.Context, scope nativeScope, finding healthFinding) error {
	publication := *scope.healthPublication
	scope.healthPublication = &publication
	digest, err := healthIssueDigest(finding)
	if err != nil {
		return err
	}
	report := healthstore.IssueReport{OrganizationID: scope.organization, FindingID: finding.ID, ProjectID: scope.project, IntakeFingerprint: finding.Fingerprint, EvidenceDigest: digest}
	publicationDigest := digest
	var previous healthstore.IssueReport
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(healthstore.IssueReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		previous, err = owner.HealthIssueReport(ctx, finding.ID, scope.project, finding.Fingerprint)
		if errors.Is(err, corestore.ErrNotFound) {
			previous, err = owner.LatestHealthIssueReport(ctx, scope.project, finding.Fingerprint)
		}
		return err
	})
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if err == nil {
		scope.healthPublication.linked = previous.WorkItemID
		issue, err := s.readWorkItem(ctx, scope, string(previous.WorkItemID))
		if err != nil {
			return err
		}
		if previous.FindingID == finding.ID && previous.EvidenceDigest == digest && previous.ResolvedAt == nil && !issue.Terminal {
			return nil
		}
		if !issue.Terminal {
			scope.healthPublication.report = &report
			request, err := healthIssueRequest(finding)
			if err != nil {
				return err
			}
			_, err = s.createNativeCommentCommand(ctx, scope, string(issue.WorkItemID), tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: healthPublicationKey(scope, finding.Fingerprint, digest)}, Body: issueorigin.Occurrence(request.Body)})
			return err
		}
		publicationDigest += "\x00renew:" + string(issue.WorkItemID)
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Background, func(r tenantstore.Reader) error {
			owner, ok := r.(healthstore.IssueReader)
			if !ok {
				return workItemStorageUnavailable()
			}
			latest, err := owner.LatestHealthIssueReport(ctx, scope.project, finding.Fingerprint)
			if err == nil {
				scope.healthPublication.linked = latest.WorkItemID
			}
			return err
		}); err != nil {
			return err
		}
	}
	request, err := healthIssueRequest(finding)
	if err != nil {
		return err
	}
	request.State = "Backlog"
	request.Mutation = tracker.Mutation{IdempotencyKey: healthPublicationKey(scope, finding.Fingerprint, publicationDigest)}
	scope.healthPublication.report = &report
	_, err = s.createNativeIssueCommand(ctx, scope, request)
	return err
}

func healthPublicationKey(scope nativeScope, fingerprint, digest string) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(scope.healthPublication.work.SourceRevision, 10) + "\x00" + scope.healthPublication.work.FindingID + "\x00" + string(scope.project) + "\x00" + fingerprint + "\x00" + digest))
	return "health_" + hex.EncodeToString(sum[:])
}

func (s *Service) resolveHealthIssueReport(ctx context.Context, scope nativeScope, finding healthFinding, report healthstore.IssueReport, now time.Time) error {
	publication := *scope.healthPublication
	scope.healthPublication = &publication
	issue, err := s.readWorkItem(ctx, scope, string(report.WorkItemID))
	if err != nil {
		return err
	}
	if !issue.Terminal && (report.ResolvedAt == nil || !report.ResolvedAt.Equal(*finding.ResolvedAt)) {
		report.ResolvedAt = finding.ResolvedAt
		scope.healthPublication.report = &report
		key := healthPublicationKey(scope, report.IntakeFingerprint, "resolved:"+formatHubTime(*finding.ResolvedAt))
		_, err := s.createNativeCommentCommand(ctx, scope, string(report.WorkItemID), tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: key}, Body: fmt.Sprintf("## Health finding resolved\n\nFinding: `%s`\nResolved: %s", finding.ID, formatHubTime(*finding.ResolvedAt))})
		if err != nil {
			return err
		}
	}
	if issue.State != "Backlog" || issue.Terminal || issue.Actor != *nativeIntegrationActor("health_detector") || issue.Provenance != nil || !healthDetectorIssueBody(issue.Body) || !now.After(finding.ResolvedAt.Add(healthReopenWindow)) {
		return nil
	}
	allowed := false
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(healthstore.IssueReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		allowed, err = owner.CanResolveHealthIssue(ctx, scope.project, issue.WorkItemID, now.Add(-healthReopenWindow))
		return err
	})
	if err != nil || !allowed {
		return err
	}
	scope.healthPublication.report = nil
	scope.healthPublication.resolve = issue.WorkItemID
	scope.healthPublication.cutoff = now.Add(-healthReopenWindow)
	err = s.cancelHealthIssue(ctx, scope, string(issue.WorkItemID), tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: healthPublicationKey(scope, report.IntakeFingerprint, "cancel:"+formatHubTime(*finding.ResolvedAt))}, ExpectedRevision: issue.Revision, State: "Cancelled", Reason: "worker_progress", ReasonDetail: "Health findings resolved for the resolution window"})
	return err
}
