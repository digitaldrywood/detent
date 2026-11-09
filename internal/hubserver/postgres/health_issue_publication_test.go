package postgres_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthIssuePublicationFencesSourceAndRetainsReports(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	now := historyAt()
	objects := newHistoryObjects()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		ref, err := objects.StoreDetail(t.Context(), string(org), []byte("{}"))
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"first", "second"} {
			project := tracker.ProjectID("prj_" + id)
			work := healthstore.IssueWork{OrganizationID: org, FindingID: id, ProjectID: project, SourceRevision: 1, PublicationRevision: 1, SourceDigest: strings.Repeat("a", 64), NextCheckAt: now}
			if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
				if _, err := w.CreateHealthRecord(t.Context(), postgres.CreateHealthRecordInput{OrganizationID: org, ID: id, Fingerprint: id, Signal: "runner_stale", Class: "instance", SubjectKind: "runner", SubjectID: id, OpenedAt: now, LastSeenAt: now, Severity: "attention", Summary: "Stale runner", NextAction: "Restore the connection", EvidenceObjectKey: ref.Key, EvidenceSha256: ref.SHA256, EvidenceBytes: ref.CompressedSize, EvidenceContentBytes: ref.UncompressedSize}); err != nil {
					return err
				}
				if err := w.LinkHealthProject(t.Context(), postgres.LinkHealthProjectInput{OrganizationID: org, FindingID: id, ProjectID: project}); err != nil {
					return err
				}
				return w.(healthstore.IssueWriter).EnqueueHealthIssueWork(t.Context(), work)
			}); err != nil {
				t.Fatal(err)
			}
			write := func(action func(healthstore.IssueWriter) error) error {
				return repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error { return action(w.(healthstore.IssueWriter)) })
			}
			if err := write(func(w healthstore.IssueWriter) error {
				var err error
				work, err = w.AdvanceHealthIssueWork(t.Context(), work, now.Add(time.Minute))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			revised := work
			revised.SourceRevision++
			revised.PublicationRevision++
			revised.SourceDigest = strings.Repeat("b", 64)
			if err := write(func(w healthstore.IssueWriter) error { return w.EnqueueHealthIssueWork(t.Context(), revised) }); err != nil {
				t.Fatal(err)
			}
			if err := write(func(w healthstore.IssueWriter) error { return w.FinishHealthIssueWork(t.Context(), work, false, now) }); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("stale completion=%v", err)
			}
			if err := write(func(w healthstore.IssueWriter) error {
				return w.FinishHealthIssueWork(t.Context(), revised, false, now)
			}); err != nil {
				t.Fatal(err)
			}
			report := healthstore.IssueReport{OrganizationID: org, FindingID: id, ProjectID: project, WorkItemID: tracker.NativeWorkItemID("wi_" + id + "_1"), IntakeFingerprint: "fingerprint", EvidenceDigest: revised.SourceDigest, ResolvedAt: &now}
			if err := write(func(w healthstore.IssueWriter) error { return w.RecordHealthIssueReport(t.Context(), report) }); err != nil {
				t.Fatal(err)
			}
			for _, item := range []tracker.NativeWorkItemID{tracker.NativeWorkItemID("wi_" + id + "_2"), report.WorkItemID} {
				report.WorkItemID = item
				if err := write(func(w healthstore.IssueWriter) error { return w.RecordHealthIssueReport(t.Context(), report) }); err != nil {
					t.Fatal(err)
				}
			}
			report.IntakeFingerprint = "second-fingerprint"
			if err := write(func(w healthstore.IssueWriter) error { return w.RecordHealthIssueReport(t.Context(), report) }); err != nil {
				t.Fatal(err)
			}
			if err := write(func(w healthstore.IssueWriter) error { return w.RecordHealthIssueReport(t.Context(), report) }); err != nil {
				t.Fatal(err)
			}
			report.OrganizationID = "org_foreign"
			if err := write(func(w healthstore.IssueWriter) error { return w.RecordHealthIssueReport(t.Context(), report) }); !errors.Is(err, corestore.ErrTenantMismatch) {
				t.Fatalf("foreign report=%v", err)
			}
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
			owner := r.(healthstore.IssueReader)
			if _, err := owner.NextHealthIssueWork(t.Context(), now.Add(time.Hour)); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("finished work returned=%v", err)
			}
			for _, id := range []string{"first", "second"} {
				rows, err := owner.HealthIssueReports(t.Context(), id, tracker.ProjectID("prj_"+id), "", 10)
				if err != nil {
					return err
				}
				if len(rows) != 2 || rows[0].OrganizationID != org || rows[0].ResolvedAt == nil || !rows[0].ResolvedAt.Equal(now) || rows[0].EvidenceDigest != strings.Repeat("b", 64) || rows[0].WorkItemID != rows[1].WorkItemID {
					t.Fatalf("retained reports=%+v", rows)
				}
				page, err := owner.HealthIssueReports(t.Context(), id, tracker.ProjectID("prj_"+id), rows[0].IntakeFingerprint, 1)
				if err != nil || len(page) != 1 || page[0].IntakeFingerprint != rows[1].IntakeFingerprint {
					t.Fatalf("receipt cursor page=%+v err=%v", page, err)
				}
				latest, err := owner.LatestHealthIssueReport(t.Context(), tracker.ProjectID("prj_"+id), rows[0].IntakeFingerprint)
				if err != nil || latest.WorkItemID != rows[0].WorkItemID || latest.OrganizationID != org {
					t.Fatalf("latest intake owner=%+v err=%v", latest, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
