package hubserver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/changefiles/sourcetest"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func changeTestSource(t *testing.T, input *tracker.ChangeVersionInput, content ...[]byte) []byte {
	t.Helper()
	return sourcetest.Source(t, input, content...)
}

func changeTestSourceCheckout(t *testing.T, input *tracker.ChangeVersionInput, content ...[]byte) ([]byte, string) {
	t.Helper()
	return sourcetest.Checkout(t, input, content...)
}

func changeTestPack(t *testing.T, kind byte, size uint64, content []byte, count uint32) []byte {
	t.Helper()
	return sourcetest.Pack(t, kind, size, content, count)
}

func validationRepository(repository string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(repository, "https://github.com/"), "git@github.com:"), ".git")
}

func historyRepairSource(t *testing.T, f changeFixture, base tracker.ChangeVersion, count int) (*neutralimporttest.Fixture, changeFixture, []tracker.ChangeVersion) {
	t.Helper()
	change := f.change
	change.ID, change.Revision, change.Landed = hubapi.NewID("change"), 1, nil
	versions := make([]tracker.ChangeVersion, count)
	for index := range versions {
		versions[index] = base
		versions[index].ID, versions[index].ChangeID, versions[index].Number = fmt.Sprintf("%s_version_%04d", change.ID, index), change.ID, int64(index+1)
	}
	change.CurrentVersion = versions[len(versions)-1].ID
	fixture := newCoreNeutralFixture(t, f.project.OrganizationID, "change_requests", "change_issue_links", "change_versions", "change_evidence", "change_sources", "change_landing_receipts", "quality_landings", "change_viewed_files", "policy_revisions", "change_command_outputs", "change_finding_attributions", "change_content_objects", "attachments", "native_pr_publications", "release_deliveries")
	fixture.Add("policy_revisions", map[string]any{"scope": string(f.project.OrganizationID) + "/" + string(f.project.ID), "policy_id": base.PolicyID, "metadata_json": string(historyTestJSON(t, base.Policy))})
	fixture.Add("change_requests", map[string]any{"id": change.ID, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": f.issue.WorkItemID, "record_json": string(historyTestJSON(t, change))})
	for _, item := range change.LinkedIssues {
		fixture.Add("change_issue_links", map[string]any{"change_id": change.ID, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": item})
	}
	var bundle []byte
	if base.Source != nil {
		row, err := f.service.readChangeVersionRecord(t.Context(), f.project.OrganizationID, f.change.ID, base.ID)
		if err != nil {
			t.Fatal(err)
		}
		bundle, err = eventstore.NewContent(f.service.runDetailStore()).SourceBundle(t.Context(), f.project.OrganizationID, row, *base.Source)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, version := range versions {
		fixture.Add("change_versions", map[string]any{"id": version.ID, "change_id": change.ID, "number": version.Number, "record_json": string(historyTestJSON(t, version)), "policy_revision_id": version.PolicyID, "repository": version.Repository})
		if version.Source != nil {
			fixture.Add("change_sources", map[string]any{"version_id": version.ID, "bundle": bundle, "object_key": "", "object_sha256": "", "object_bytes": 0, "content_bytes": 0})
		}
	}
	f.change, f.path = change, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/changes/"+change.ID
	return fixture, f, versions
}

func historyRepairImportSource(t *testing.T, target changeFixture, fixture *neutralimporttest.Fixture) coreimport.CoreChangeImportSource {
	t.Helper()
	objects := target.service.runDetailStore()
	return coreimport.CoreChangeImportSource{BundlePath: fixture.Write(), OrganizationID: target.project.OrganizationID, SourceObjects: objects, Objects: objects}
}

func historyRepairImport(t *testing.T, target changeFixture, source coreimport.CoreChangeImportSource, count int64) {
	t.Helper()
	got, err := coreimport.ImportCoreChanges(t.Context(), target.service.config.TenantRepository.(*postgres.Repository), source)
	if err != nil || got != count {
		t.Fatalf("historical repair import count=%d want=%d error=%v", got, count, err)
	}
}
