package searchrecords_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDuplicateFingerprintProbesMatchSameDefect(t *testing.T) {
	t.Parallel()
	f := newBoardSearchFixture(t)
	ctx := t.Context()
	cloud := tracker.ProjectID(issueorigin.DetentCloudProjectID)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedBoardSearchProject(t, f, org, cloud, "example", []tracker.NativeWorkItemID{"wi_cloud_1"})
	}
	if _, err := f.admin.ExecContext(ctx, `INSERT INTO native_work_item_page_progress(organization_id,backfill_phase) VALUES('org_first',2),('org_second',2) ON CONFLICT (organization_id) DO UPDATE SET backfill_phase=2`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(ctx, `DELETE FROM native_work_item_page_dirty WHERE kind='item'`); err != nil {
		t.Fatal(err)
	}
	stamp := func(body, kind, fingerprint string) string {
		return issueorigin.Stamp(body, issueorigin.Origin{Kind: kind, Instance: "runner", Source: "attempt", Fingerprint: fingerprint})
	}
	release := func(series string) string {
		return issueorigin.Stamp("Release stabilization: `"+series+"`", issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/digitaldrywood/detent/actions/runs/1", Fingerprint: issueorigin.ReleaseStabilizationFingerprint(series)})
	}
	report := func(series string) string {
		body := "Problem: `go-test:example/pkg:TestOne`\n"
		origin := issueorigin.Origin{Kind: "worker", Instance: "runner", Source: "example-run", Fingerprint: issueorigin.Fingerprint("go-test:example/pkg:TestOne")}
		if series != "" {
			body += "Release stabilization: `" + series + "`\n"
			origin = issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/digitaldrywood/detent/actions/runs/1/attempts/1", Fingerprint: issueorigin.ReleaseStabilizationFingerprint(series)}
		}
		return issueorigin.Stamp(body, origin)
	}
	failure := "--- FAIL: TestJourney (0.01s)\nFAIL example/hub 0.02s"
	problem := "Problem: `go-test:example/hub:TestJourney`"
	defect := issueorigin.DefectFingerprint(problem)
	for _, test := range []struct {
		name     string
		project  tracker.ProjectID
		previous string
		incoming string
	}{
		{"same origin", "prj_first", stamp("Report", "doctor", "example-one"), stamp("Again", "doctor", "example-one")},
		{"different origin", "prj_first", stamp("Report", "doctor", "example-one"), stamp("Other", "doctor", "example-two")},
		{"worker report of an owner's defect key", "prj_first", stamp(problem, "doctor", "invented"), stamp(failure, "worker", "worker-key")},
		{"doctor report of an owner's defect key", "prj_first", stamp(problem, "doctor", "invented"), stamp(failure, "doctor", "doctor-key")},
		{"worker owner of the incoming defect key", "prj_first", stamp(problem, "worker", "invented"), stamp(failure, "doctor", "doctor-key")},
		{"incoming origin is the owner's defect key", "prj_first", stamp(problem, "worker", "invented"), stamp("Report", "doctor", defect)},
		{"owner origin is an incoming defect key", "prj_first", stamp("Report", "doctor", defect), stamp(failure, "worker", "worker-key")},
		{"unstamped owner", "prj_first", problem, stamp(failure, "worker", "worker-key")},
		{"same release series", cloud, release("release/v1.2"), release("release/v1.2")},
		{"different release series", cloud, release("release/v1.2"), release("release/v1.3")},
		{"series against plain origin", cloud, release("release/v1.2"), stamp("Report", "doctor", issueorigin.ReleaseStabilizationFingerprint("release/v1.2"))},
		{"plain origins in the cloud project", cloud, stamp("Report", "doctor", "example-one"), stamp("Again", "doctor", "example-one")},
		{"release cannot reuse ordinary repair", cloud, report(""), report("release/v1.2")},
		{"ordinary repair cannot reuse release", cloud, report("release/v1.2"), report("")},
		{"same release occurrence", cloud, report("release/v1.2"), report("release/v1.2")},
		{"different release occurrence", cloud, report("release/v1.2"), report("release/v1.3")},
		{"other project keeps defect matching", "prj_first", report(""), report("release/v1.2")},
	} {
		item := tracker.NativeWorkItemID("wi_first_1")
		if test.project == cloud {
			item = "wi_cloud_1"
		}
		want, _ := searchstore.MatchDuplicateSource(searchstore.DuplicateSource{Document: searchstore.Document{ProjectID: test.project, WorkItemID: item, ID: "issue:" + string(item), Kind: "issue", Scope: "issue", Revision: 1}, Text: searchstore.Text{Title: "Example", Body: test.previous}}, searchstore.Text{Title: "Unrelatedheading", Body: test.incoming})
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			if _, err := f.admin.ExecContext(ctx, `DELETE FROM native_work_item_fingerprints WHERE organization_id=$1`, org); err != nil {
				t.Fatal(err)
			}
			for _, row := range searchstore.FingerprintRows(test.project, test.previous) {
				if _, err := f.admin.ExecContext(ctx, `INSERT INTO native_work_item_fingerprints(organization_id,project_id,work_item_id,kind,record_id,fingerprint_digest,is_origin,origin_kind,release_series) VALUES($1,$2,$3,'comment','cmt_previous',$4,$5,$6,$7)`, org, test.project, item, row.Digest, row.Origin, row.OriginKind, row.ReleaseSeries); err != nil {
					t.Fatal(err)
				}
			}
			var page searchstore.DuplicateSourcePage
			if err := f.repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				page, err = r.(searchstore.DuplicateSourceReader).DuplicateSources(ctx, searchstore.DuplicateSourceQuery{ProjectID: test.project, Request: &searchstore.Text{Title: "Unrelatedheading", Body: test.incoming}, Text: "Unrelatedheading\n" + test.incoming, Machine: true, Now: pgstoretest.HistoryAt()})
				return err
			}); err != nil {
				t.Fatalf("tenant %s %s: %v", org, test.name, err)
			}
			got := false
			for _, source := range page.Sources {
				if source.Document.OrganizationID != org {
					t.Fatalf("tenant %s %s read a foreign source %+v", org, test.name, source)
				}
				got = got || source.Document.WorkItemID == item && source.Fingerprint
			}
			if got != want.Fingerprint {
				t.Errorf("tenant %s %s: typed probe match=%t, SameDefect=%t", org, test.name, got, want.Fingerprint)
			}
		}
	}
}
