package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeLandingColumnsPreserveHistoricalReads(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC)
	clock := atomic.Pointer[time.Time]{}
	clock.Store(new(at.Add(-2 * time.Hour)))
	f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return *clock.Load() }})
	items := []tracker.NativeIssue{f.create(t, "First historical item"), f.create(t, "Second historical item")}
	scope := analyticsTestScope(t, f)
	clock.Store(new(at.Add(5 * time.Hour)))
	objects := f.service.runDetailStore()
	pause := errors.New("historical landing import paused")
	interrupted := &landingImportPauseDetails{DetailStore: objects, changeID: "historical_change_b", pause: pause}
	sourceFixture := newCoreNeutralFixture(t, scope.Organization, "change_requests", "change_issue_links", "change_versions", "change_evidence", "change_sources", "change_landing_receipts", "quality_landings", "change_viewed_files", "policy_revisions", "change_command_outputs", "change_finding_attributions", "change_content_objects", "attachments", "native_pr_publications", "release_deliveries")
	descriptor := hubTestPolicy()
	sourceFixture.Add("policy_revisions", map[string]any{"scope": string(scope.Organization) + "/" + string(scope.Project), "policy_id": descriptor.ID, "metadata_json": string(historyTestJSON(t, descriptor))})
	expected := map[string]string{}
	for i, suffix := range []string{"a", "b"} {
		id := "historical_change_" + suffix
		attempt := "historical-source-" + suffix
		seedAnalyticsTestAttempt(t, hostedPostgresAdmin(t, f.service), f.service, scope, items[i].WorkItemID, attempt, "succeeded", at.Add(-2*time.Minute), at, tracker.NativeRunData{})
		actor := tracker.Actor{Kind: "human", PrincipalID: "operator"}
		version := tracker.ChangeVersion{ID: "historical_version_" + suffix, ChangeID: id, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), Repository: "owner/repository", PolicyID: descriptor.ID, AttemptID: attempt}, Actor: actor, CreatedAt: at.Add(-time.Minute)}
		landing := tracker.ChangeLanding{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: strings.Repeat(strconv.Itoa(i+1), 40), BaseRef: "develop", LandedAt: at, Actor: actor}
		change := tracker.ChangeRequest{ID: id, OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: items[i].WorkItemID, LinkedIssues: []tracker.NativeWorkItemID{items[i].WorkItemID}, Title: "Historical change", CurrentVersion: version.ID, Revision: 1, CreatedAt: at.Add(-time.Hour), UpdatedAt: at}
		if i == 0 {
			change.Landed = &landing
		}
		sourceFixture.Add("change_requests", map[string]any{"id": id, "organization_id": scope.Organization, "project_id": scope.Project, "work_item_id": items[i].WorkItemID, "record_json": string(historyTestJSON(t, change)), "created_at": nil, "landed_at": nil, "landed_version_id": nil, "landed_head_sha": nil, "landed_merge_sha": nil, "landed_base_ref": nil})
		sourceFixture.Add("change_issue_links", map[string]any{"change_id": id, "organization_id": scope.Organization, "project_id": scope.Project, "work_item_id": items[i].WorkItemID})
		sourceFixture.Add("change_versions", map[string]any{"id": version.ID, "change_id": id, "number": int64(1), "policy_revision_id": descriptor.ID, "record_json": string(historyTestJSON(t, version)), "head_sha": nil, "repository": version.Repository})
		receipt := tracker.NativeLandingReceipt{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: landing.MergeSHA, BaseRef: landing.BaseRef, Landed: true, ObservedAt: at}
		sourceFixture.Add("change_landing_receipts", map[string]any{"attempt_id": attempt, "version_id": version.ID, "record_json": string(historyTestJSON(t, receipt))})
		if i == 1 {
			sourceFixture.Add("quality_landings", map[string]any{"version_id": version.ID, "record_json": string(historyTestJSON(t, landing)), "landed_at": nil})
		}
		expected[id] = version.ID
	}
	source := coreimport.CoreChangeImportSource{BundlePath: sourceFixture.Write(), OrganizationID: scope.Organization, SourceObjects: objects, Objects: interrupted}
	repository := f.service.config.TenantRepository.(*postgres.Repository)
	imported, err := coreimport.ImportCoreChanges(t.Context(), repository, source)
	if !errors.Is(err, pause) || imported != 1 {
		t.Fatalf("partial historical import count=%d error=%v", imported, err)
	}
	window := operatortool.AnalyticsWindow{From: at.Add(-time.Hour), To: at.Add(time.Hour)}
	read := func(name string, w operatortool.AnalyticsWindow, reopened []tracker.NativeWorkItemID) []nativeAnalyticsLanding {
		t.Helper()
		population, err := f.service.readAnalyticsLandingPopulationTenant(t.Context(), scope, w, name, reopened, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range population.Items {
			if row.Landing.VersionID != expected[row.ChangeID] || !row.Landing.LandedAt.Equal(at) {
				t.Fatalf("lost exact historical landing=%+v", row)
			}
		}
		return population.Items
	}
	for _, name := range []string{"landings", "quality_landings"} {
		if rows := read(name, window, nil); len(rows) != 1 || rows[0].ChangeID != "historical_change_a" {
			t.Fatalf("pending=true landings=%+v want=1", rows)
		}
	}
	interrupted.pause = nil
	if imported, err := coreimport.ImportCoreChanges(t.Context(), repository, source); err != nil || imported != 2 {
		t.Fatalf("resumed historical import count=%d error=%v", imported, err)
	}
	for range 2 {
		for _, name := range []string{"landings", "quality_landings"} {
			if rows := read(name, window, nil); len(rows) != 2 {
				t.Fatalf("pending=false landings=%+v want=2", rows)
			}
		}
		if _, err := coreimport.ImportCoreChanges(t.Context(), repository, source); err != nil {
			t.Fatal(err)
		}
	}
	unprovenFixture := newNativeFixture(t, f.service, scope.Organization+"_second", "Unproven project")
	unprovenItem := unprovenFixture.create(t, "Unproven historical item")
	unproven := analyticsTestScope(t, unprovenFixture)
	unprovenVersion := tracker.ChangeVersion{ID: "unproven_historical_version", ChangeID: "unproven_historical_change", Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), Repository: "owner/repository", PolicyID: descriptor.ID}, Policy: descriptor, Actor: tracker.Actor{Kind: "human", PrincipalID: "operator"}, CreatedAt: at.Add(-time.Minute)}
	unprovenLanding := tracker.ChangeLanding{VersionID: unprovenVersion.ID, HeadSHA: unprovenVersion.HeadSHA, MergeSHA: strings.Repeat("4", 40), BaseRef: "develop", LandedAt: at, Actor: unprovenVersion.Actor}
	unprovenChange := tracker.ChangeRequest{ID: unprovenVersion.ChangeID, OrganizationID: unproven.Organization, ProjectID: unproven.Project, WorkItemID: unprovenItem.WorkItemID, LinkedIssues: []tracker.NativeWorkItemID{unprovenItem.WorkItemID}, Revision: 1, Title: "Unproven imported landing", CurrentVersion: unprovenVersion.ID, Landed: &unprovenLanding, CreatedAt: at.Add(-time.Hour), UpdatedAt: at}
	unprovenContent := eventstore.NewContent(objects)
	unprovenInitial := unprovenChange
	unprovenInitial.CurrentVersion = ""
	unprovenChangeRecord, err := unprovenContent.PrepareChange(t.Context(), unproven.Organization, historyTestJSON(t, unprovenInitial))
	if err != nil {
		t.Fatal(err)
	}
	unprovenVersionRecord, err := unprovenContent.PrepareVersion(t.Context(), unproven.Organization, historyTestJSON(t, unprovenVersion))
	if err != nil {
		t.Fatal(err)
	}
	unprovenVersionRecord.LandingHistoryKnown = false
	if err := repository.WriteRecords(t.Context(), unproven.Organization, corestore.Background, func(w eventstore.Writer) error {
		if _, err := w.CreateChange(t.Context(), unprovenChangeRecord); err != nil {
			return err
		}
		if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: unproven.Organization, ProjectID: unproven.Project, WorkItemID: unprovenItem.WorkItemID, ChangeID: unprovenChange.ID}); err != nil {
			return err
		}
		_, err := w.CreateVersion(t.Context(), unprovenVersionRecord)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), unproven.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		change, err := r.Change(t.Context(), unprovenChange.ID)
		if err != nil {
			return err
		}
		version, err := r.Version(t.Context(), unprovenChange.ID, unprovenVersion.ID)
		if err != nil {
			return err
		}
		if change.LandedVersionID == nil || *change.LandedVersionID != unprovenVersion.ID || version.LandingHistoryKnown || version.FrozenLanding != nil {
			t.Fatalf("fixture lost actual unproven import state: change=%+v version=%+v", change, version)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, repository, commit, branch, change string
		scope                                    hubscope.NativeScope
	}{
		{"matching first", "owner/repository", strings.Repeat("1", 40), "develop", "historical_change_a", scope},
		{"interrupted imported landing has no provenance", "owner/repository", strings.Repeat("4", 40), "develop", "", unproven},
		{"interrupted imported landing has no history provenance", "owner/repository", strings.Repeat("4", 40), "", "", unproven},
		{"different commit", "owner/repository", strings.Repeat("3", 40), "develop", "", scope},
		{"different branch", "owner/repository", strings.Repeat("1", 40), "feature", "", scope},
		{"different repository", "other/repository", strings.Repeat("1", 40), "develop", "", scope},
		{"later matching landing", "owner/repository", strings.Repeat("2", 40), "develop", "historical_change_b", scope},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got eventstore.PolicyLanding
			err := repository.ReadTenant(t.Context(), test.scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				got, err = r.ProjectPolicyLanding(t.Context(), test.scope.Project, test.repository, test.commit, test.branch)
				return err
			})
			if test.change == "" {
				if !errors.Is(err, corestore.ErrNotFound) || got.ChangeID != "" {
					t.Fatalf("landing=%+v want change=%q error=%v", got, test.change, err)
				}
				return
			}
			if err != nil || got.ChangeID != test.change {
				t.Fatalf("landing=%+v want change=%q error=%v", got, test.change, err)
			}
		})
	}
	for _, item := range items {
		appendAnalyticsTestEvent(t, f.service, scope, item.WorkItemID, "workflow.transitioned", tracker.CollaborationData{FromState: "Done", ToState: "Rework"}, at.Add(3*time.Hour))
	}
	reopenedWindow := operatortool.AnalyticsWindow{From: at.Add(2 * time.Hour), To: at.Add(4 * time.Hour)}
	for _, pending := range []bool{true, false} {
		if !pending {
			if _, err := coreimport.ImportCoreChanges(t.Context(), repository, source); err != nil {
				t.Fatal(err)
			}
		}
		inputs, err := f.service.readAnalyticsQualityInputsTenant(t.Context(), scope, reopenedWindow)
		if err != nil || len(inputs.Landings) != 2 {
			t.Fatalf("pending events=%v lost reopened historical landings: rows=%v error=%v", pending, inputs.Landings, err)
		}
	}
}

type landingImportPauseDetails struct {
	attachment.DetailStore
	changeID string
	pause    error
}

func (s *landingImportPauseDetails) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	var value tracker.ChangeRequest
	if s.pause != nil && json.Unmarshal(raw, &value) == nil && value.ID == s.changeID && value.Revision > 0 {
		return attachment.ArchiveReference{}, s.pause
	}
	return s.DetailStore.StoreDetail(ctx, organization, raw)
}
