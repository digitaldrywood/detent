package searchrecords_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthWriterContractPublishesRecurrenceLinksAndTicks(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	now := pgstoretest.HistoryAt()
	objects := pgstoretest.NewHistoryObjects()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, repository, org)
		evidence, err := objects.StoreDetail(t.Context(), string(org), []byte("example evidence"))
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"first", "second"} {
			if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
				row, err := w.CreateHealthRecord(t.Context(), postgres.CreateHealthRecordInput{OrganizationID: org, ID: id, Fingerprint: id, Signal: "signal", Class: "class", SubjectKind: "work_item", SubjectID: "wi_" + id + "_1", OpenedAt: now, LastSeenAt: now, Severity: "attention", Summary: string(org), NextAction: "inspect", EvidenceObjectKey: evidence.Key, EvidenceSha256: evidence.SHA256, EvidenceBytes: evidence.CompressedSize, EvidenceContentBytes: evidence.UncompressedSize})
				if err != nil {
					return err
				}
				if row.Revision != 1 {
					t.Fatalf("revision=%d", row.Revision)
				}
				if err := w.LinkHealthProject(t.Context(), postgres.LinkHealthProjectInput{OrganizationID: org, FindingID: id, ProjectID: tracker.ProjectID("prj_" + id)}); err != nil {
					return err
				}
				if err := w.LinkHealthIssue(t.Context(), postgres.LinkHealthIssueInput{OrganizationID: org, FindingID: id, ProjectID: tracker.ProjectID("prj_" + id), WorkItemID: "wi_" + id + "_1"}); err != nil {
					return err
				}
				if err := w.PutHealthBaseline(t.Context(), postgres.PutHealthBaselineInput{OrganizationID: org, ProjectID: tracker.ProjectID("prj_" + id), Signal: "signal", Lane: "Todo"}); err != nil {
					return err
				}
				return w.PutHealthTick(t.Context(), postgres.PutHealthTickInput{OrganizationID: org, LastTickAt: now})
			}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO projects(organization_id,id,name,profile) SELECT $1,'prj_wide_'||lpad(n::text,3,'0'),'Example '||n,'native' FROM generate_series(1,201) n`, org); err != nil {
			t.Fatal(err)
		}
		wide := make([]string, 201)
		for i := range wide {
			wide[i] = fmt.Sprintf("prj_wide_%03d", i+1)
		}
		replaceProjects := func(w postgres.HealthWriter, projects []string) error {
			if err := w.ReplaceHealthProjectLinks(t.Context(), postgres.ReplaceHealthProjectLinksInput{OrganizationID: org, FindingID: "first", ProjectIds: projects}); err != nil {
				return err
			}
			for _, project := range projects {
				if err := w.LinkHealthProject(t.Context(), postgres.LinkHealthProjectInput{OrganizationID: org, FindingID: "first", ProjectID: tracker.ProjectID(project)}); err != nil {
					return err
				}
			}
			return nil
		}
		readProjects := func() []string {
			t.Helper()
			var projects []string
			err := repository.ReadHealth(t.Context(), org, corestore.Serving, func(r postgres.HealthReader) error {
				after := tracker.ProjectID("")
				for {
					rows, err := r.HealthRecordProjects(t.Context(), postgres.HealthRecordProjectsInput{OrganizationID: org, FindingID: "first", AfterProjectID: after, Limit: corestore.MaxPageSize})
					if err != nil {
						return err
					}
					for _, row := range rows {
						if row.OrganizationID != org || row.FindingID != "first" {
							t.Fatalf("foreign project link=%+v", row)
						}
						projects = append(projects, string(row.ProjectID))
					}
					if len(rows) < corestore.MaxPageSize {
						return nil
					}
					after = rows[len(rows)-1].ProjectID
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			return projects
		}
		if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
			return replaceProjects(w, wide)
		}); err != nil {
			t.Fatal(err)
		}
		want := append([]string{"prj_first"}, wide...)
		if got := readProjects(); !slices.Equal(got, want) {
			t.Fatalf("wide finding links=%v want=%v", got, want)
		}
		retained := []string{wide[0], wide[len(wide)-1]}
		if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
			return replaceProjects(w, retained)
		}); err != nil {
			t.Fatal(err)
		}
		want = append([]string{"prj_first"}, retained...)
		if got := readProjects(); !slices.Equal(got, want) {
			t.Fatalf("shrunk finding lost requested or filed links=%v want=%v", got, want)
		}
		rollback := errors.New("rollback finding publication")
		err = repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
			_, err := w.UpdateHealthRecord(t.Context(), postgres.UpdateHealthRecordInput{OrganizationID: org, ID: "first", ExpectedRevision: 1, LastSeenAt: now.Add(30 * time.Second), Severity: "attention", Summary: string(org), NextAction: "inspect", EvidenceObjectKey: evidence.Key, EvidenceSha256: evidence.SHA256, EvidenceBytes: evidence.CompressedSize, EvidenceContentBytes: evidence.UncompressedSize})
			if err != nil {
				return err
			}
			if err := replaceProjects(w, nil); err != nil {
				return err
			}
			return rollback
		})
		if !errors.Is(err, rollback) {
			t.Fatalf("publication rollback=%v", err)
		}
		if got := readProjects(); !slices.Equal(got, want) {
			t.Fatalf("failed publication changed links=%v want=%v", got, want)
		}
		if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
			return replaceProjects(w, nil)
		}); err != nil {
			t.Fatal(err)
		}
		if got := readProjects(); !slices.Equal(got, []string{"prj_first"}) {
			t.Fatalf("empty scope lost filed link=%v", got)
		}
		update := postgres.UpdateHealthRecordInput{OrganizationID: org, ID: "first", ExpectedRevision: 1, LastSeenAt: now.Add(time.Minute), ResolvedAt: new(now.Add(time.Minute)), Severity: "attention", Summary: string(org), NextAction: "inspect", EvidenceObjectKey: evidence.Key, EvidenceSha256: evidence.SHA256, EvidenceBytes: evidence.CompressedSize, EvidenceContentBytes: evidence.UncompressedSize}
		if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error { _, err := w.UpdateHealthRecord(t.Context(), update); return err }); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error { _, err := w.UpdateHealthRecord(t.Context(), update); return err }); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("stale update=%v", err)
		}
		update.ExpectedRevision = 2
		update.ResolvedAt = nil
		update.LastSeenAt = now.Add(2 * time.Minute)
		if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
			row, err := w.UpdateHealthRecord(t.Context(), update)
			if err != nil {
				return err
			}
			if row.Revision != 3 || row.ResolvedAt != nil {
				t.Fatalf("reopen=%+v", row)
			}
			if err := w.PutHealthTick(t.Context(), postgres.PutHealthTickInput{OrganizationID: org, LastTickAt: update.LastSeenAt}); err != nil {
				return err
			}
			return w.PutHealthTick(t.Context(), postgres.PutHealthTickInput{OrganizationID: org, LastTickAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
				candidate, err := w.NextHealthPage(t.Context())
				if err != nil {
					return err
				}
				published, err := w.PublishHealthPage(t.Context(), candidate, healthstore.Evidence{Reason: "example"})
				if err != nil {
					return err
				}
				if !published {
					t.Fatal("produced health row not published")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	err := repository.WriteHealth(t.Context(), "org_first", corestore.Background, func(w postgres.HealthWriter) error {
		return w.LinkHealthIssue(t.Context(), postgres.LinkHealthIssueInput{OrganizationID: "org_second", FindingID: "first", ProjectID: "prj_first", WorkItemID: "wi_first_1"})
	})
	if !errors.Is(err, corestore.ErrTenantMismatch) {
		t.Fatalf("foreign link=%v", err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadHealth(t.Context(), org, corestore.Serving, func(r postgres.HealthReader) error {
			tick, err := r.HealthTick(t.Context())
			if err != nil {
				return err
			}
			if !tick.Equal(now.Add(2 * time.Minute)) {
				t.Fatalf("tick=%v", tick)
			}
			for _, id := range []string{"first", "second"} {
				row, err := r.HealthRecord(t.Context(), postgres.HealthRecordInput{OrganizationID: org, ID: id})
				if err != nil {
					return err
				}
				if row.OrganizationID != org || row.Summary != string(org) {
					t.Fatalf("finding=%+v", row)
				}
				found, err := r.RecentHealthFingerprint(t.Context(), postgres.RecentHealthFingerprintInput{OrganizationID: org, Fingerprint: id, ResolvedAt: new(now)})
				if err != nil {
					return err
				}
				if found.ID != id {
					t.Fatalf("fingerprint=%+v", found)
				}
				links, err := r.HealthRecordIssues(t.Context(), postgres.HealthRecordIssuesInput{OrganizationID: org, FindingID: id, Limit: 2})
				if err != nil {
					return err
				}
				if len(links) != 1 || links[0].OrganizationID != org || links[0].WorkItemID != "wi_"+id+"_1" {
					t.Fatalf("links=%+v", links)
				}
				page, err := r.HealthFindings(t.Context(), tracker.ProjectID("prj_"+id), "open", nil, 0, "", 10)
				if err != nil {
					return err
				}
				if len(page.Findings) != 1 || page.Findings[0].ID != id {
					t.Fatalf("produced public page=%+v", page)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
