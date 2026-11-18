package healthprojection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errHealthOutsideExaminedScope = errors.New("health finding is outside examined scope")

type Scope struct {
	RetiredSubjects map[healthsignals.Subject]bool
	ObservedAt      time.Time
	Fingerprints    map[string]bool
	Projects        map[tracker.ProjectID]bool
	Runners         map[string]bool
	Complete        bool
}

func (scope Scope) coversFinding(finding healthsignals.Finding) bool {
	if scope.Fingerprints != nil && !scope.Fingerprints[finding.Fingerprint] {
		return false
	}
	if scope.Complete {
		return true
	}
	if finding.Subject.ID == "" {
		return false
	}
	switch finding.Subject.Kind {
	case "runner":
		return scope.Runners[finding.Subject.ID]
	case "project":
		if !scope.Projects[tracker.ProjectID(finding.Subject.ID)] {
			return false
		}
	case "work_item":
		if len(finding.Projects) == 0 {
			return false
		}
	default:
		return false
	}
	for _, project := range finding.Projects {
		if !scope.Projects[tracker.ProjectID(project)] {
			return false
		}
	}
	return true
}

func (scope Scope) CoversRecord(ctx context.Context, reader postgres.HealthReader, organization tracker.OrganizationID, row postgres.HealthRecord) (bool, error) {
	if scope.Fingerprints != nil && !scope.Fingerprints[row.Fingerprint] {
		return false, nil
	}
	if scope.Complete {
		return true, nil
	}
	if row.SubjectID == "" {
		return false, nil
	}
	if scope.RetiredSubjects[healthsignals.Subject{Kind: row.SubjectKind, ID: row.SubjectID}] {
		if scope.ObservedAt.IsZero() {
			return false, corestore.ErrInvalidPage
		}
		owner, ok := reader.(tenantstore.Reader)
		if !ok {
			return false, hubapi.NativeWorkItemStorageUnavailable()
		}
		switch row.SubjectKind {
		case "work_item":
			_, err := owner.WorkItemByNativeID(ctx, tracker.NativeWorkItemID(row.SubjectID))
			if errors.Is(err, corestore.ErrNotFound) {
				return true, nil
			}
			return false, err
		case "project":
			project, err := owner.Project(ctx, tracker.ProjectID(row.SubjectID))
			if errors.Is(err, corestore.ErrNotFound) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			return project.DeletedAt != nil || project.Profile != "native", nil
		case "runner":
			runner, err := owner.RunnerIdentity(ctx, row.SubjectID)
			if errors.Is(err, corestore.ErrNotFound) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			credential, err := owner.AccessCredential(ctx, runner.TokenID)
			if errors.Is(err, corestore.ErrNotFound) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			return runner.RemovedAt != nil || !runner.ExpiresAt.After(scope.ObservedAt) || !credential.Valid(credential.Hash, scope.ObservedAt) || credential.Scope != "worker" || credential.Kind != "service", nil
		}
	}

	switch row.SubjectKind {
	case "runner":
		return scope.Runners[row.SubjectID], nil
	case "project":
		if !scope.Projects[tracker.ProjectID(row.SubjectID)] {
			return false, nil
		}
	case "work_item":
	default:
		return false, nil
	}
	after := tracker.ProjectID("")
	hasProject := row.SubjectKind == "project"
	for {
		links, err := reader.HealthRecordProjects(ctx, postgres.HealthRecordProjectsInput{OrganizationID: organization, FindingID: row.ID, AfterProjectID: after, Limit: corestore.MaxPageSize})
		if err != nil {
			return false, err
		}
		for _, link := range links {
			hasProject = true
			if !scope.Projects[link.ProjectID] {
				return false, nil
			}
		}
		if len(links) < corestore.MaxPageSize {
			return hasProject, nil
		}
		after = links[len(links)-1].ProjectID
	}
}

func (s *Service) Publish(ctx context.Context, organization tracker.OrganizationID, at time.Time, findings []healthsignals.Finding, scope Scope) error {
	if !scope.Complete && len(scope.Projects) == 0 && len(scope.Runners) == 0 {
		return corestore.ErrInvalidPage
	}
	if scope.Fingerprints != nil {
		if scope.Complete || len(scope.Fingerprints) == 0 || len(scope.Fingerprints) > healthsignals.ReadLimit {
			return corestore.ErrInvalidPage
		}
		for fingerprint, examined := range scope.Fingerprints {
			if fingerprint == "" || !examined {
				return corestore.ErrInvalidPage
			}
		}
	}
	for project, examined := range scope.Projects {
		if project == "" || !examined {
			return corestore.ErrInvalidPage
		}
	}
	for runner, examined := range scope.Runners {
		if runner == "" || !examined {
			return corestore.ErrInvalidPage
		}
	}
	if s.Postgres == nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	details := s.Details
	if len(findings) > 0 && details == nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	if scope.Fingerprints != nil {
		if err := hubmaintenance.RequireAllowance(ctx, string(organization), hubmaintenance.ByteLimit); err != nil {
			return err
		}
		details = hubmaintenance.LimitDetails(details)
	}
	if len(findings) > healthsignals.ReadLimit {
		return healthsignals.ErrReadLimit
	}
	seen := make(map[string]bool, len(findings))
	for _, finding := range findings {
		if !scope.coversFinding(finding) {
			return corestore.ErrInvalidPage
		}
		if seen[finding.Fingerprint] {
			return errors.New("duplicate health fingerprint")
		}
		seen[finding.Fingerprint] = true
	}
	if !scope.Complete && len(findings) != 0 {
		err := s.Postgres.ReadHealth(ctx, organization, corestore.Background, func(r postgres.HealthReader) error {
			cutoff := at.Add(-healthsignals.ReopenWindow)
			for _, finding := range findings {
				row, err := r.RecentHealthFingerprint(ctx, postgres.RecentHealthFingerprintInput{OrganizationID: organization, Fingerprint: finding.Fingerprint, ResolvedAt: &cutoff})
				if errors.Is(err, corestore.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				covered, err := scope.CoversRecord(ctx, r, organization, row)
				if err != nil {
					return err
				}
				if !covered {
					return corestore.ErrInvalidPage
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for _, finding := range findings {
		raw, err := json.Marshal(finding.Evidence)
		if err != nil {
			return fmt.Errorf("encode health evidence: %w", err)
		}
		ref, err := details.StoreDetail(ctx, string(organization), raw)
		if err != nil {
			return fmt.Errorf("store health evidence: %w", err)
		}
		if err := s.publishHealthFinding(ctx, organization, at, finding, ref, scope); err != nil {
			return err
		}
	}
	if scope.Fingerprints != nil {
		keys := make([]string, 0, len(scope.Fingerprints))
		for fingerprint := range scope.Fingerprints {
			keys = append(keys, fingerprint)
		}
		slices.Sort(keys)
		for _, fingerprint := range keys {
			if seen[fingerprint] {
				continue
			}
			var row postgres.HealthRecord
			err := s.Postgres.ReadHealth(ctx, organization, corestore.Background, func(reader postgres.HealthReader) error {
				var err error
				row, err = reader.RecentHealthFingerprint(ctx, postgres.RecentHealthFingerprintInput{OrganizationID: organization, Fingerprint: fingerprint, ResolvedAt: new(at.Add(-healthsignals.ReopenWindow))})
				return err
			})
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if row.ResolvedAt != nil || row.LastSeenAt.After(at) {
				continue
			}
			if err := s.Resolve(ctx, organization, at, row, scope); err != nil {
				return err
			}
		}
		return nil
	}
	after := ""
	for remaining := healthsignals.ReadLimit; ; {
		pageLimit := min(remaining+1, 200)
		var rows []postgres.HealthRecord
		err := s.Postgres.ReadHealth(ctx, organization, corestore.Background, func(r postgres.HealthReader) error {
			var err error
			rows, err = r.HealthEvaluationRecords(ctx, postgres.HealthEvaluationRecordsInput{OrganizationID: organization, ID: after, Limit: int32(pageLimit)})
			return err
		})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		if len(rows) > remaining {
			return healthsignals.ErrReadLimit
		}
		remaining -= len(rows)
		after = rows[len(rows)-1].ID
		for _, row := range rows {
			if seen[row.Fingerprint] || row.LastSeenAt.After(at) {
				continue
			}
			if err := s.Resolve(ctx, organization, at, row, scope); err != nil {
				return err
			}
		}
		if len(rows) < pageLimit {
			break
		}
	}
	if !scope.Complete {
		return nil
	}
	return s.Postgres.WriteHealth(ctx, organization, corestore.Background, func(w postgres.HealthWriter) error {
		return w.PutHealthTick(ctx, postgres.PutHealthTickInput{OrganizationID: organization, LastTickAt: at})
	})
}

func (s *Service) publishHealthFinding(ctx context.Context, organization tracker.OrganizationID, at time.Time, finding healthsignals.Finding, ref attachment.ArchiveReference, scope Scope) error {
	return s.Postgres.WriteHealth(ctx, organization, corestore.Background, func(w postgres.HealthWriter) error {
		cutoff := at.Add(-healthsignals.ReopenWindow)
		row, err := w.RecentHealthFingerprint(ctx, postgres.RecentHealthFingerprintInput{OrganizationID: organization, Fingerprint: finding.Fingerprint, ResolvedAt: &cutoff})
		created := errors.Is(err, corestore.ErrNotFound)
		if err != nil && !created {
			return err
		}
		if !created && (row.LastSeenAt.After(at) || row.ResolvedAt != nil && row.ResolvedAt.After(at)) {
			return corestore.ErrRevisionConflict
		}
		if created {
			opened := at
			if !finding.ConditionSince.IsZero() && finding.ConditionSince.Before(at) {
				opened = finding.ConditionSince
			}
			row, err = w.CreateHealthRecord(ctx, postgres.CreateHealthRecordInput{OrganizationID: organization, ID: hubapi.NewID("finding"), Fingerprint: finding.Fingerprint, Signal: finding.Signal, Class: finding.Class, SubjectKind: finding.Subject.Kind, SubjectID: finding.Subject.ID, OpenedAt: opened, LastSeenAt: at, Severity: finding.Severity, Summary: finding.Summary, NextAction: finding.NextAction, EvidenceObjectKey: ref.Key, EvidenceSha256: ref.SHA256, EvidenceBytes: ref.CompressedSize, EvidenceContentBytes: ref.UncompressedSize})
		} else {
			row, err = w.UpdateHealthRecord(ctx, postgres.UpdateHealthRecordInput{OrganizationID: organization, ID: row.ID, ExpectedRevision: row.Revision, LastSeenAt: at, Severity: finding.Severity, Summary: finding.Summary, NextAction: finding.NextAction, EvidenceObjectKey: ref.Key, EvidenceSha256: ref.SHA256, EvidenceBytes: ref.CompressedSize, EvidenceContentBytes: ref.UncompressedSize})
		}
		if err != nil {
			return err
		}
		if !created {
			covered, err := scope.CoversRecord(ctx, w, organization, row)
			if err != nil {
				return err
			}
			if !covered {
				return corestore.ErrInvalidPage
			}
		}
		projects := slices.Clone(finding.Projects)
		slices.Sort(projects)
		projects = slices.Compact(projects)
		if err := w.ReplaceHealthProjectLinks(ctx, postgres.ReplaceHealthProjectLinksInput{OrganizationID: organization, FindingID: row.ID, ProjectIds: projects}); err != nil {
			return err
		}
		for _, project := range projects {
			if err := w.LinkHealthProject(ctx, postgres.LinkHealthProjectInput{OrganizationID: organization, FindingID: row.ID, ProjectID: tracker.ProjectID(project)}); err != nil {
				return err
			}
		}
		if created && row.Severity == "attention" {
			return s.enqueueSlackHealthRecord(ctx, w, organization, row, "opened", at)
		}
		return nil
	})
}

func (s *Service) Resolve(ctx context.Context, organization tracker.OrganizationID, at time.Time, row postgres.HealthRecord, scope Scope) error {
	if !scope.Complete {
		switch row.SubjectKind {
		case "project":
			if !scope.Projects[tracker.ProjectID(row.SubjectID)] {
				return nil
			}
		case "work_item":
		case "runner":
			if !scope.Runners[row.SubjectID] {
				return nil
			}
		default:
			return nil
		}
	}
	err := s.Postgres.WriteHealth(ctx, organization, corestore.Background, func(w postgres.HealthWriter) error {
		updated, err := w.UpdateHealthRecord(ctx, postgres.UpdateHealthRecordInput{OrganizationID: organization, ID: row.ID, ExpectedRevision: row.Revision, LastSeenAt: row.LastSeenAt, ResolvedAt: &at, Severity: row.Severity, Summary: row.Summary, NextAction: row.NextAction, EvidenceObjectKey: row.EvidenceObjectKey, EvidenceSha256: row.EvidenceSha256, EvidenceBytes: row.EvidenceBytes, EvidenceContentBytes: row.EvidenceContentBytes})
		if err != nil {
			return err
		}
		covered, err := scope.CoversRecord(ctx, w, organization, updated)
		if err != nil {
			return err
		}
		if !covered {
			return errHealthOutsideExaminedScope
		}
		if row.Severity == "attention" {
			return s.enqueueSlackHealthRecord(ctx, w, organization, updated, "resolved", at)
		}
		return nil
	})
	if errors.Is(err, errHealthOutsideExaminedScope) {
		return nil
	}
	return err
}

func (s *Service) enqueueSlackHealthRecord(ctx context.Context, owner postgres.HealthWriter, organization tracker.OrganizationID, row postgres.HealthRecord, event string, at time.Time) error {
	slack, ok := owner.(postgres.SlackWriter)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	settings, err := slack.SlackIntegration(ctx)
	if errors.Is(err, corestore.ErrNotFound) || err == nil && !settings.Enabled {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = slack.AccessSecret(ctx, "", s.SlackWebhookSecret)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if event == "resolved" {
		opened := sha256.Sum256([]byte(row.ID + "\x00opened"))
		_, err := slack.SlackDelivery(ctx, postgres.SlackDeliveryInput{OrganizationID: organization, ID: "slack_" + hex.EncodeToString(opened[:16])})
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	sum := sha256.Sum256([]byte(row.ID + "\x00" + event))
	id := "slack_" + hex.EncodeToString(sum[:16])
	if _, err := slack.SlackDelivery(ctx, postgres.SlackDeliveryInput{OrganizationID: organization, ID: id}); err == nil {
		return nil
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	_, err = slack.EnqueueSlackDelivery(ctx, postgres.EnqueueSlackDeliveryInput{OrganizationID: organization, ID: id, FindingID: row.ID, Event: event, EventAt: at, QueuedAt: at})
	return err
}
