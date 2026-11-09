package hubserver

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
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) publishHealthEvaluation(ctx context.Context, organization tracker.OrganizationID, at time.Time, findings []healthFinding, sources ...healthEvaluationSource) error {
	if len(sources) > 1 {
		return corestore.ErrInvalidPage
	}
	source := healthEvaluationSource{}
	if len(sources) == 1 {
		source = sources[0]
	}
	input, err := newHealthEvaluationInput(at, findings, source)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode health evaluation: %w", err)
	}
	ref, err := s.runDetailStore().StoreDetail(ctx, string(organization), raw)
	if err != nil {
		return fmt.Errorf("store health evaluation: %w", err)
	}
	work := healthstore.Evaluation{OrganizationID: organization, ID: newNativeID("health_eval"), At: input.At, Input: ref, FindingCount: int64(len(input.Findings)), BaselineCount: int64(len(input.Baselines))}
	if err := s.database.postgres.WriteHealth(ctx, organization, corestore.Background, func(w postgres.HealthWriter) error {
		owner, ok := w.(healthstore.EvaluationWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		_, err := owner.StartHealthEvaluation(ctx, work)
		return err
	}); err != nil {
		return err
	}
	if hubmaintenance.RequireBudget(ctx, string(organization)) == nil {
		return nil
	}
	for {
		worked, err := s.advanceHealthEvaluation(ctx, organization, hubmaintenance.ItemLimit)
		if err != nil {
			return err
		}
		if !worked {
			return nil
		}
	}
}

func (s *Service) publishHealthFinding(ctx context.Context, w postgres.HealthWriter, organization tracker.OrganizationID, at time.Time, finding healthFinding, ref attachment.ArchiveReference) error {
	cutoff := at.Add(-healthReopenWindow)
	row, err := w.RecentHealthFingerprint(ctx, postgres.RecentHealthFingerprintInput{OrganizationID: organization, Fingerprint: finding.Fingerprint, ResolvedAt: &cutoff})
	created := errors.Is(err, corestore.ErrNotFound)
	if err != nil && !created {
		return err
	}
	if !created && (row.LastSeenAt.After(at) || row.ResolvedAt != nil && row.ResolvedAt.After(at)) {
		return nil
	}
	if created {
		opened := at
		if !finding.ConditionSince.IsZero() && finding.ConditionSince.Before(at) {
			opened = finding.ConditionSince
		}
		row, err = w.CreateHealthRecord(ctx, postgres.CreateHealthRecordInput{OrganizationID: organization, ID: newNativeID("finding"), Fingerprint: finding.Fingerprint, Signal: finding.Signal, Class: finding.Class, SubjectKind: finding.Subject.Kind, SubjectID: finding.Subject.ID, OpenedAt: opened, LastSeenAt: at, Severity: finding.Severity, Summary: finding.Summary, NextAction: finding.NextAction, EvidenceObjectKey: ref.Key, EvidenceSha256: ref.SHA256, EvidenceBytes: ref.CompressedSize, EvidenceContentBytes: ref.UncompressedSize})
	} else {
		row, err = w.UpdateHealthRecord(ctx, postgres.UpdateHealthRecordInput{OrganizationID: organization, ID: row.ID, ExpectedRevision: row.Revision, LastSeenAt: at, Severity: finding.Severity, Summary: finding.Summary, NextAction: finding.NextAction, EvidenceObjectKey: ref.Key, EvidenceSha256: ref.SHA256, EvidenceBytes: ref.CompressedSize, EvidenceContentBytes: ref.UncompressedSize})
	}
	if err != nil {
		return err
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
	if err := enqueueHealthIssueRecord(ctx, w, organization, row, finding, at); err != nil {
		return err
	}
	if created && row.Severity == "attention" {
		return enqueueSlackHealthRecord(ctx, w, organization, row, "opened", at)
	}
	return nil
}

func (s *Service) resolveHealthRecord(ctx context.Context, w postgres.HealthWriter, organization tracker.OrganizationID, at time.Time, row postgres.HealthRecord) error {
	updated, err := w.UpdateHealthRecord(ctx, postgres.UpdateHealthRecordInput{OrganizationID: organization, ID: row.ID, ExpectedRevision: row.Revision, LastSeenAt: row.LastSeenAt, ResolvedAt: &at, Severity: row.Severity, Summary: row.Summary, NextAction: row.NextAction, EvidenceObjectKey: row.EvidenceObjectKey, EvidenceSha256: row.EvidenceSha256, EvidenceBytes: row.EvidenceBytes, EvidenceContentBytes: row.EvidenceContentBytes})
	if err != nil {
		return err
	}
	finding := healthFinding{Fingerprint: row.Fingerprint, Class: row.Class, Subject: healthSubject{Kind: row.SubjectKind, ID: row.SubjectID}, Severity: row.Severity}
	if err := enqueueHealthIssueRecord(ctx, w, organization, updated, finding, at); err != nil {
		return err
	}
	if row.Severity == "attention" {
		return enqueueSlackHealthRecord(ctx, w, organization, updated, "resolved", at)
	}
	return nil
}

func enqueueSlackHealthRecord(ctx context.Context, owner postgres.HealthWriter, organization tracker.OrganizationID, row postgres.HealthRecord, event string, at time.Time) error {
	slack, ok := owner.(postgres.SlackWriter)
	if !ok {
		return workItemStorageUnavailable()
	}
	settings, err := slack.SlackIntegration(ctx)
	if errors.Is(err, corestore.ErrNotFound) || err == nil && !settings.Enabled {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = slack.AccessSecret(ctx, "", slackWebhookSecret)
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
