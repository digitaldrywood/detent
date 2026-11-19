package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (f changeFixture) anotherChange(t *testing.T) changeFixture {
	t.Helper()
	issue := f.create(t, hubapi.NewID("other-work"))
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
	response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("other-change")}, Title: "Other change"})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	return changeFixture{nativeFixture: f.nativeFixture, issue: issue, change: change, path: path + "/" + change.ID, rules: f.rules}
}

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

func TestChangeColumnsSelectIssueSideData(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	for _, anchor := range []changeFixture{first, first.anotherChange(t)} {
		for _, test := range []struct {
			name string
			pull bool
		}{
			{"current version", false}, {"version head", false}, {"current landing", false}, {"old landing", false}, {"external reference", true}, {"external provider", false}, {"empty external URL", false},
		} {
			for _, pending := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/pending=%v", anchor.change.ID, test.name, pending), func(t *testing.T) {
					base := anchor
					if pending {
						base = newChangeFixture(t, nil)
					}
					f := base.anotherChange(t)
					old := f.publish(t, "old", "")
					current := f.publish(t, "current", old.ID)
					head, branch := current.HeadSHA, ""
					if test.name == "current version" {
						head = old.HeadSHA
					}
					if test.name == "version head" && !pending {
						head = "typed-head"
					}
					if test.name == "current landing" {
						head, branch = "typed-merge", "typed-base"
					}
					f = changeColumnSideDataFixture(t, f, old, current, test.name, pending)
					snapshot, err := f.service.boardProjectionSnapshot(t.Context(), f.project.OrganizationID, viewstore.BoardCandidate{ProjectID: f.project.ID, WorkItemID: f.issue.WorkItemID, Required: true}, f.service.config.Clock())
					if err != nil {
						t.Fatal(err)
					}
					issue := f.issue
					f.service.prepareNativeIssueChangeRecord(snapshot, &issue)
					if issue.ListChange == nil || issue.ListChange.HeadSHA != head || issue.ListChange.Branch != branch {
						t.Fatalf("pending=%v issue Change=%+v want branch=%s head=%s", pending, issue.ListChange, branch, head)
					}
					if (issue.PullRequest != nil) != test.pull || test.pull && (issue.PullRequest.Number != 42 || issue.PullRequest.URL != "https://example.test/pull/42") {
						t.Fatalf("pending=%v issue PR=%+v", pending, issue.PullRequest)
					}
					if test.name == "current version" {
						candidate := viewstore.BoardCandidate{ProjectID: f.project.ID, WorkItemID: f.issue.WorkItemID, Required: true}
						if err := f.service.refreshBoardRecord(t.Context(), f.project.OrganizationID, candidate, f.service.config.Clock()); err != nil {
							t.Fatal(err)
						}
						public := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "?include=change")
						if public.ListChange == nil || public.ListChange.HeadSHA != head || public.ListChange.Branch != branch {
							t.Fatalf("pending=%v public change=%+v want branch=%s head=%s", pending, public.ListChange, branch, head)
						}
						if public.Change == nil || public.Change.ChangeID != f.change.ID || public.PullRequest != nil {
							t.Fatalf("pending=%v public change=%+v PR=%+v", pending, public.Change, public.PullRequest)
						}
					}
				})
			}
		}
	}
}

func changeColumnSideDataFixture(t *testing.T, f changeFixture, old, current tracker.ChangeVersion, scenario string, historical bool) changeFixture {
	t.Helper()
	content := eventstore.NewContent(f.service.runDetailStore())
	external := func(version *tracker.ChangeVersion) {
		switch scenario {
		case "external reference":
			version.External = &tracker.ChangeExternalReference{Provider: "github", ID: "42", URL: "https://example.test/pull/42"}
		case "external provider":
			version.External = &tracker.ChangeExternalReference{Provider: "other", ID: "42", URL: "https://example.test/pull/42"}
		case "empty external URL":
			version.External = &tracker.ChangeExternalReference{Provider: "github", ID: "42", URL: ""}
		}
	}
	if historical {
		fixture, target, versions := historyRepairSource(t, f, old, 2)
		versions[1].HeadSHA = current.HeadSHA
		external(&versions[1])
		target.change.CreatedAt, target.change.UpdatedAt = f.service.config.Clock(), f.service.config.Clock()
		if scenario == "current version" {
			target.change.CurrentVersion = versions[0].ID
		}
		if scenario == "current landing" || scenario == "old landing" {
			landed := versions[1]
			if scenario == "old landing" {
				landed = versions[0]
			}
			target.change.Landed = &tracker.ChangeLanding{VersionID: landed.ID, HeadSHA: landed.HeadSHA, MergeSHA: "typed-merge", BaseRef: "typed-base", LandedAt: f.service.config.Clock()}
		}
		fixture.Records["change_requests"] = nil
		fixture.Add("change_requests", map[string]any{"id": target.change.ID, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": f.issue.WorkItemID, "created_at": nil, "record_json": string(historyTestJSON(t, target.change))})
		fixture.Records["change_versions"] = nil
		for _, version := range versions {
			fixture.Add("change_versions", map[string]any{"id": version.ID, "change_id": target.change.ID, "number": version.Number, "policy_revision_id": version.PolicyID, "head_sha": nil, "record_json": string(historyTestJSON(t, version))})
		}
		historyRepairImport(t, target, historyRepairImportSource(t, target, fixture), 1)
		return target
	}
	current.ID, current.Number = hubapi.NewID("version"), current.Number+1
	row, err := content.PrepareVersion(t.Context(), f.project.OrganizationID, historyTestJSON(t, current))
	if err != nil {
		t.Fatal(err)
	}
	switch scenario {
	case "version head":
		row.HeadSHA = "typed-head"
	case "external reference":
		row.ExternalProvider, row.ExternalID, row.ExternalURL = "github", "42", "https://example.test/pull/42"
	case "external provider":
		row.ExternalProvider, row.ExternalID, row.ExternalURL = "other", "42", "https://example.test/pull/42"
	case "empty external URL":
		row.ExternalProvider, row.ExternalID, row.ExternalURL = "github", "42", ""
	}
	change, err := f.service.readChangeRecord(t.Context(), analyticsTestScope(t, f.nativeFixture), string(f.issue.WorkItemID), f.change.ID)
	if err != nil {
		t.Fatal(err)
	}
	change.CurrentVersion = current.ID
	changed, err := content.PrepareChange(t.Context(), f.project.OrganizationID, historyTestJSON(t, change))
	if err != nil {
		t.Fatal(err)
	}
	if scenario == "current version" {
		changed.CurrentVersionID = new(old.ID)
	}
	if scenario == "current landing" || scenario == "old landing" {
		landed := current
		if scenario == "old landing" {
			landed = old
		}
		changed.LandedVersionID, changed.LandedHeadSHA = new(landed.ID), new(landed.HeadSHA)
		changed.LandedMergeSHA, changed.LandedBaseRef = new("typed-merge"), new("typed-base")
		changed.LandedAt = new(f.service.config.Clock())
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateVersion(t.Context(), row); err != nil {
			return err
		}
		_, err := w.UpdateChange(t.Context(), changed)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestChangeColumnsSelectEscapeOccurrence(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC)
	service := newPostgresNativeService(t, Config{Clock: func() time.Time { return at }})
	for _, pending := range []bool{false, true} {
		native := newNativeFixture(t, service, "", fmt.Sprintf("escape pending=%v", pending))
		states := append(nativeFixtureStates(), tracker.NativeState{Name: "Merging"}, tracker.NativeState{Name: "Rework"})
		setChangeFixtureStates(t, native, states)
		var items []tracker.NativeIssue
		events := make(map[tracker.NativeWorkItemID][3]string)
		escape := func(id string, observed time.Time) *tracker.QualityEscape {
			return &tracker.QualityEscape{OccurrenceID: id, Kind: "reopened", Cause: "infrastructure", ObservedAt: observed, InstanceID: "example-instance", EvidenceReference: "history/" + id, EvidenceQuote: "Recorded workflow transition", BasisQuote: "Recorded instance failure"}
		}
		fixtureScope := analyticsTestScope(t, native)
		escapeDiscussions := func(old, version tracker.ChangeVersion, ids [3]string) []tracker.ChangeDiscussion {
			return []tracker.ChangeDiscussion{{ID: "discussion-0", VersionID: version.ID, Actor: fixtureScope.Actor(), CreatedAt: at, Escape: escape(ids[0], at)}, {ID: "discussion-1", VersionID: version.ID, Actor: fixtureScope.Actor(), CreatedAt: at, Escape: escape("unrelated", at)}, {ID: "ordinary", VersionID: version.ID, Actor: fixtureScope.Actor(), CreatedAt: at}, {ID: "other version", VersionID: old.ID, Actor: fixtureScope.Actor(), CreatedAt: at, Escape: escape(ids[0], at)}}
		}
		historical := make(map[tracker.NativeWorkItemID][2]tracker.ChangeVersion)
		historicalChanges := make(map[tracker.NativeWorkItemID]tracker.ChangeRequest)
		if pending {
			descriptor := hubTestPolicy()
			items, events, _ = changeColumnHistoricalWorkflow(t, native, at, func(fixture *neutralimporttest.Fixture, ids map[tracker.NativeWorkItemID][3]string) {
				for name, table := range newCoreNeutralFixture(t, fixtureScope.Organization, "change_requests", "change_issue_links", "change_versions", "change_evidence", "change_sources", "change_landing_receipts", "quality_landings", "change_viewed_files", "policy_revisions", "change_command_outputs", "change_finding_attributions", "change_content_objects", "attachments", "native_pr_publications", "release_deliveries").Tables {
					fixture.Tables[name] = table
				}
				fixture.Add("policy_revisions", map[string]any{"scope": string(fixtureScope.Organization) + "/" + string(fixtureScope.Project), "policy_id": descriptor.ID, "metadata_json": string(historyTestJSON(t, descriptor))})
				for _, item := range []tracker.NativeWorkItemID{"wi_core_import", "wi_core_import_second"} {
					changeID := "historical_change_" + string(item)
					var versions [2]tracker.ChangeVersion
					for i, head := range []string{strings.Repeat("b", 40), strings.Repeat("c", 40)} {
						versions[i] = tracker.ChangeVersion{ID: fmt.Sprintf("%s_version_%d", changeID, i+1), ChangeID: changeID, Number: int64(i + 1), ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: head, Repository: "owner/repository", PolicyID: descriptor.ID}, Policy: descriptor, Actor: fixtureScope.Actor(), CreatedAt: at}
						fixture.Add("change_versions", map[string]any{"id": versions[i].ID, "change_id": changeID, "number": versions[i].Number, "policy_revision_id": descriptor.ID, "repository": versions[i].Repository, "record_json": string(historyTestJSON(t, versions[i]))})
					}
					change := tracker.ChangeRequest{ID: changeID, OrganizationID: fixtureScope.Organization, ProjectID: fixtureScope.Project, WorkItemID: item, LinkedIssues: []tracker.NativeWorkItemID{item}, Title: "Historical change", CurrentVersion: versions[1].ID, Revision: 1, CreatedAt: at, UpdatedAt: at}
					fixture.Add("change_requests", map[string]any{"id": changeID, "organization_id": fixtureScope.Organization, "project_id": fixtureScope.Project, "work_item_id": item, "created_at": nil, "record_json": string(historyTestJSON(t, change))})
					fixture.Add("change_issue_links", map[string]any{"change_id": changeID, "organization_id": fixtureScope.Organization, "project_id": fixtureScope.Project, "work_item_id": item})
					for i, discussion := range escapeDiscussions(versions[0], versions[1], ids[item]) {
						fixture.Add("change_evidence", map[string]any{"sequence": i + 2, "change_id": changeID, "version_id": discussion.VersionID, "kind": "discussion", "source_key": nil, "record_json": string(historyTestJSON(t, discussion)), "escape_occurrence_id": nil, "escape_observed_at": nil})
					}
					historical[item] = versions
					historicalChanges[item] = change
				}
			})
			content := eventstore.NewContent(native.service.runDetailStore())
			repository := native.service.config.TenantRepository.(*postgres.Repository)
			if len(historicalChanges) != 2 {
				t.Fatalf("historical change import: %v", eventstore.ErrInvalidRecord)
			}
			for item, change := range historicalChanges {
				initial := change
				initial.CurrentVersion = ""
				record, err := content.PrepareChange(t.Context(), fixtureScope.Organization, historyTestJSON(t, initial))
				if err != nil {
					t.Fatalf("historical change import: %v", err)
				}
				var versions []eventstore.Version
				for _, version := range historical[item] {
					row, err := content.PrepareVersion(t.Context(), fixtureScope.Organization, historyTestJSON(t, version))
					if err != nil {
						t.Fatalf("historical change import: %v", err)
					}
					row.LandingHistoryKnown = false
					versions = append(versions, row)
				}
				if err := repository.WriteRecords(t.Context(), fixtureScope.Organization, corestore.Background, func(w eventstore.Writer) error {
					if _, err := w.CreateChange(t.Context(), record); err != nil {
						return err
					}
					if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: fixtureScope.Organization, ProjectID: fixtureScope.Project, WorkItemID: item, ChangeID: change.ID}); err != nil {
						return err
					}
					for i, version := range versions {
						if _, err := w.CreateVersion(t.Context(), version); err != nil {
							return err
						}
						if err := w.ConfirmVersionUnlanded(t.Context(), change.ID, version.ID, historical[item][i].HeadSHA); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatalf("historical change import: %v", err)
				}
			}
		} else {
			items = []tracker.NativeIssue{native.create(t, "first escape"), native.create(t, "second escape")}
			for _, item := range items {
				events[item.WorkItemID] = changeColumnWorkflow(t, native, item, at)
			}
		}
		for _, item := range items {
			t.Run(fmt.Sprintf("%s/pending=%v", item.WorkItemID, pending), func(t *testing.T) {
				var f changeFixture
				var old, version tracker.ChangeVersion
				if pending {
					old, version = historical[item.WorkItemID][0], historical[item.WorkItemID][1]
					f = changeFixture{nativeFixture: native, issue: item, change: tracker.ChangeRequest{ID: old.ChangeID}}
				} else {
					f = changeColumnCreateChange(t, native, item)
					old = f.publish(t, "old", "")
					version = f.publish(t, "current", old.ID)
				}
				scope := analyticsTestScope(t, f.nativeFixture)
				ids := events[item.WorkItemID]
				content := eventstore.NewContent(f.service.runDetailStore())
				landing, err := content.PrepareVersionLanding(t.Context(), eventstore.VersionLanding{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item.WorkItemID, ChangeID: f.change.ID, VersionID: version.ID}, tracker.ChangeLanding{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: strings.Repeat("a", 40), BaseRef: "develop", LandedAt: at})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { return w.RecordVersionLanding(t.Context(), landing) }); err != nil {
					t.Fatal(err)
				}
				discussions := escapeDiscussions(old, version, ids)
				wanted := discussions[0]
				tampered, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: f.change.ID, Kind: "discussion"}, historyTestJSON(t, wanted))
				if err != nil {
					t.Fatal(err)
				}
				tampered.ReportEscape.OccurrenceID = "changed"
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), tampered); return err }); err != nil {
					t.Fatal(err)
				}
				lookup := func(version, occurrence string) (eventstore.Evidence, error) {
					var row eventstore.Evidence
					err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						row, err = r.(eventstore.QualityEscapeReader).QualityEscapeOccurrence(t.Context(), f.change.ID, version, occurrence)
						return err
					})
					return row, err
				}
				if row, err := lookup(version.ID, ids[0]); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("typed occurrence ignored: row=%+v error=%v", row, err)
				}
				if pending {
					for i, discussion := range discussions {
						row, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: f.change.ID, Sequence: int64(i + 2), VersionID: &discussion.VersionID, Kind: "discussion"}, historyTestJSON(t, discussion))
						if err != nil {
							t.Fatal(err)
						}
						if err := f.service.config.TenantRepository.(*postgres.Repository).WriteRecords(t.Context(), scope.Organization, corestore.Background, func(w eventstore.Writer) error {
							_, err := w.AppendEvidence(t.Context(), row)
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					for _, discussion := range discussions {
						row, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: f.change.ID, Kind: "discussion"}, historyTestJSON(t, discussion))
						if err != nil {
							t.Fatal(err)
						}
						if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), row); return err }); err != nil {
							t.Fatal(err)
						}
					}
				}
				change, err := f.service.readChangeRecord(t.Context(), scope, string(item.WorkItemID), f.change.ID)
				if err != nil {
					t.Fatal(err)
				}
				for i, id := range ids {
					_, err := f.service.prepareQualityEscapeTenant(t.Context(), scope, change, tracker.DiscussChange{VersionID: version.ID, Escape: escape(id, at.Add(time.Duration(i)*time.Second))}, at.Add(3*time.Second))
					if (err == nil) != (i == 0) {
						t.Fatalf("pending=%v event=%d reopened=%v error=%v", pending, i, err == nil, err)
					}
				}
				recovery, err := f.service.readNativeRecoveryTenant(t.Context(), scope, string(item.WorkItemID))
				if err != nil || len(recovery.History) != 1 || recovery.History[0].ID != ids[1] {
					t.Fatalf("pending=%v landing exit=%+v error=%v", pending, recovery.History, err)
				}
				row, err := lookup(version.ID, ids[0])
				if err != nil {
					t.Fatal(err)
				}
				raw, err := content.Evidence(t.Context(), row)
				var discussion tracker.ChangeDiscussion
				if err != nil || json.Unmarshal(raw, &discussion) != nil || discussion.ID != "discussion-0" {
					t.Fatalf("pending=%v escape=%+v error=%v", pending, discussion, err)
				}
				if _, err := lookup(version.ID, "missing"); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("pending=%v unrelated occurrence matched: %v", pending, err)
				}
				if row, err := lookup(old.ID, ids[0]); err != nil || row.VersionID == nil || *row.VersionID != old.ID {
					t.Fatalf("version scope lost: %+v error=%v", row, err)
				}
			})
		}
	}
}

func changeColumnCreateChange(t *testing.T, f nativeFixture, issue tracker.NativeIssue) changeFixture {
	t.Helper()
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	rules := tracker.ChangeReviewPolicy{PolicyID: hubTestPolicy().ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}
	approved := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "column-rules"}, Policy: rules})
	requireNativeStatus(t, approved, http.StatusOK)
	decodeHubResponse(t, approved, &rules)
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
	response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Column evidence"})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	return changeFixture{nativeFixture: f, issue: issue, change: change, path: path + "/" + change.ID, rules: rules}
}

func changeColumnWorkflow(t *testing.T, f nativeFixture, item tracker.NativeIssue, at time.Time) [3]string {
	t.Helper()
	var ids [3]string
	for i, data := range []tracker.CollaborationData{{FromState: "Done", ToState: "Todo"}, {FromState: "Merging", ToState: "Rework"}, {FromState: "Todo", ToState: "In Progress"}} {
		data.Revision = item.Revision
		value := tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateID: item.WorkItemID, AggregateType: "issue", Type: "workflow.transitioned", SchemaVersion: 1, Actor: analyticsTestScope(t, f).Actor(), RecordedAt: at.Add(time.Duration(i) * time.Second), Data: data}
		row, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), f.project.OrganizationID, historyTestJSON(t, value))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), row); return err }); err != nil {
			t.Fatal(err)
		}
		ids[i] = value.ID
	}
	return ids
}

func changeColumnHistoricalWorkflow(t *testing.T, f nativeFixture, at time.Time, extend func(*neutralimporttest.Fixture, map[tracker.NativeWorkItemID][3]string)) ([]tracker.NativeIssue, map[tracker.NativeWorkItemID][3]string, string) {
	t.Helper()
	body := nativeContractTestBody + "\n" + strings.Repeat("x", 8192)
	fixture := coreWorkItemNeutralFixtureBody(t, f.project.OrganizationID, f.project.ID, false, body)
	fixture.Add("workflow_states", map[string]any{"id": 2, "project_id": f.project.ID, "detent_state": "In Progress"})
	actor := tracker.Actor{Kind: "human", PrincipalID: "example-owner"}
	ids := make(map[tracker.NativeWorkItemID][3]string)
	fixture.Records["collaboration_versions"] = nil
	fixture.Records["native_comments"] = nil
	for index, suffix := range []string{"", "_second"} {
		item := tracker.NativeWorkItemID("wi_core_import" + suffix)
		var eventIDs [3]string
		for i, data := range []tracker.CollaborationData{{FromState: "Done", ToState: "Todo"}, {FromState: "Merging", ToState: "Rework"}, {FromState: "Todo", ToState: "In Progress"}} {
			data.Revision = 2
			eventIDs[i] = fmt.Sprintf("%s_transition_%d", item, i)
			fixture.Add("collaboration_events", map[string]any{"id": eventIDs[i], "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": item, "sequence": i + 3, "type": "workflow.transitioned", "schema_version": 1, "actor_json": string(historyTestJSON(t, actor)), "data_json": string(historyTestJSON(t, data)), "event_revision": 2, "from_state": nil, "to_state": nil, "recorded_at": hubapi.FormatHubTime(at.Add(time.Duration(i) * time.Second))})
		}
		ids[item] = eventIDs
		for revision := 1; revision <= 2; revision++ {
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: item, Number: 1 + index*2, Revision: tracker.Revision(revision), Profile: "native"}, Title: "Initial title", Body: body, State: "Todo", Labels: []string{"needs-review"}, Assignees: []string{"example-assignee"}, Actor: actor, CreatedAt: parseTestTime(t, "2026-10-01T00:00:00Z"), UpdatedAt: parseTestTime(t, "2026-10-01T00:00:00Z"), LastActivityAt: parseTestTime(t, "2026-10-01T00:00:00Z")}
			if revision == 2 {
				issue.Title, issue.State = "Updated title", "In Progress"
				issue.UpdatedAt, issue.LastActivityAt = at.Add(2*time.Second), at.Add(2*time.Second)
			}
			fixture.Add("collaboration_versions", map[string]any{"organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": item, "record_id": item, "revision": revision, "encoding": "snapshot", "record_json": string(historyTestJSON(t, issue))})
		}
	}
	for i, row := range fixture.Records["issues"] {
		for key, value := range map[string]string{"workflow_state_id": "2", "event_sequence": "5", "updated_at": hubapi.FormatHubTime(at.Add(2 * time.Second)), "native_updated_at": hubapi.FormatHubTime(at.Add(2 * time.Second)), "last_activity_at": hubapi.FormatHubTime(at.Add(2 * time.Second))} {
			row.Values[key] = storagecatalog.Cell{Encoding: "utf8", Data: []byte(value)}
		}
		fixture.Records["issues"][i] = row
	}
	for i, row := range fixture.Records["queue_entries"] {
		row.Values["state"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte("In Progress")}
		row.Values["updated_at"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte(hubapi.FormatHubTime(at.Add(2 * time.Second)))}
		fixture.Records["queue_entries"][i] = row
	}
	extend(fixture, ids)
	source := coreimport.CoreWorkItemImportSource{BundlePath: fixture.Write(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Objects: f.service.runDetailStore()}
	if n, err := coreimport.ImportCoreWorkItems(t.Context(), f.service.config.TenantRepository.(*postgres.Repository), source); err != nil || n != 2 {
		t.Fatalf("historical workflow import=%d error=%v", n, err)
	}
	var items []tracker.NativeIssue
	for _, id := range []tracker.NativeWorkItemID{"wi_core_import", "wi_core_import_second"} {
		issue, err := f.service.readWorkItem(t.Context(), analyticsTestScope(t, f), string(id))
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, issue)
	}
	return items, ids, source.BundlePath
}

func TestChangeColumnsSelectAttemptEvidence(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	for _, anchor := range []changeFixture{first, first.anotherChange(t)} {
		for _, state := range []string{"present", "absent", "unknown"} {
			t.Run(state+"/"+anchor.change.ID, func(t *testing.T) {
				base := anchor
				if state == "unknown" {
					base = newChangeFixture(t, nil)
				}
				f := base.anotherChange(t)
				old := f.publish(t, "old", "")
				current := f.publish(t, "current", old.ID)
				receipts := []tracker.NativeLandingReceipt{{ChangeID: f.change.ID, VersionID: old.ID, HeadSHA: old.HeadSHA, MergeSHA: strings.Repeat("1", 40), BaseRef: "develop", Landed: true, ObservedAt: f.service.config.Clock()}, {ChangeID: f.change.ID, VersionID: current.ID, HeadSHA: current.HeadSHA, MergeSHA: strings.Repeat("2", 40), BaseRef: "develop", Landed: true, ObservedAt: f.service.config.Clock()}}
				attempts := []leasestore.Attempt{changeColumnAttempt(t, f, &receipts[0], 100, state == "present" || state == "absent", false), changeColumnAttempt(t, f, &receipts[1], 101, state == "present", false)}
				changeColumnAttempt(t, f, nil, 102, false, false)
				if state == "unknown" {
					changeColumnImportLandingReceipts(t, f, []tracker.ChangeVersion{old, current}, attempts, receipts)
				}
				want := attempts[1].ID
				if state == "absent" {
					want = attempts[0].ID
				}
				scope := analyticsTestScope(t, f.nativeFixture)
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					row, err := r.LatestLandingReceipt(t.Context(), scope.Project, f.issue.WorkItemID)
					if err != nil {
						return err
					}
					if row.AttemptID != want {
						t.Fatalf("landing attempt=%s want=%s", row.AttemptID, want)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
		for _, test := range []struct {
			name           string
			historical     bool
			oldMerge, want string
		}{
			{"typed priority", false, strings.Repeat("1", 40), "old"}, {"typed mismatch", false, strings.Repeat("3", 40), "current"}, {"unknown matching receipt ranks first", true, strings.Repeat("1", 40), "old"},
		} {
			t.Run(test.name+"/"+anchor.change.ID, func(t *testing.T) {
				base := anchor
				if test.historical {
					base = newChangeFixture(t, nil)
				}
				f := base.anotherChange(t)
				old := f.publish(t, "old", "")
				current := f.publish(t, "current", old.ID)
				receipts := []tracker.NativeLandingReceipt{{ChangeID: f.change.ID, VersionID: old.ID, HeadSHA: old.HeadSHA, MergeSHA: test.oldMerge, BaseRef: "develop", Landed: true, ObservedAt: f.service.config.Clock()}, {ChangeID: f.change.ID, VersionID: current.ID, HeadSHA: current.HeadSHA, MergeSHA: strings.Repeat("2", 40), BaseRef: "develop", Landed: true, ObservedAt: f.service.config.Clock()}}
				attempts := []leasestore.Attempt{changeColumnAttempt(t, f, &receipts[0], 100, !test.historical, false), changeColumnAttempt(t, f, &receipts[1], 101, !test.historical, false)}
				if test.historical {
					changeColumnImportLandingReceipts(t, f, []tracker.ChangeVersion{old, current}, attempts, receipts)
				}
				repository := validationRepository(current.Repository)
				scheduled := gate.ScheduledEvidence{Schema: 1, Repository: repository, OccurrenceKey: artifact.Digest([]byte(f.change.ID)), RunID: "123", RunAttempt: "1", JobID: "456", JobName: "Check", RunURL: "https://github.com/" + repository + "/actions/runs/123/attempts/1", JobURL: "https://github.com/" + repository + "/actions/jobs/456", HeadSHA: strings.Repeat("1", 40), Conclusion: "failure"}
				body := issueorigin.Stamp("Scheduled check failed"+scheduled.Stamp(), issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/" + repository + "/actions/runs/123", Fingerprint: scheduled.OccurrenceKey})
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "scheduled-source"}, Body: body}), http.StatusOK)
				audit, err := f.service.readValidationAuditTenant(t.Context(), analyticsTestScope(t, f.nativeFixture), string(f.issue.WorkItemID), string(f.issue.WorkItemID), f.change.ID, "")
				if err != nil || audit == nil || !audit.Partial || !slices.Equal(audit.Unavailable, []string{"unlanded_historical_versions", "historical_repository_identity"}) || len(audit.Occurrences) != 1 {
					t.Fatalf("validation candidates=%+v error=%v", audit, err)
				}
				rows := audit.Occurrences[0].Comparisons
				want := old.ID
				if test.want == "current" {
					want = current.ID
				}
				if len(rows) != 2 || rows[0].VersionID != want {
					t.Fatalf("candidates=%+v want first=%s", rows, want)
				}
				for _, row := range rows {
					local := int64(100)
					if row.VersionID == current.ID {
						local = 101
					}
					if row.LocalAttemptID != local {
						t.Fatalf("local attempt=%d want=%d", row.LocalAttemptID, local)
					}
				}
			})
		}
		t.Run("typed receipt mismatch fails integrity/"+anchor.change.ID, func(t *testing.T) {
			f := anchor.anotherChange(t)
			version := f.publish(t, "version", "")
			receipt := tracker.NativeLandingReceipt{ChangeID: f.change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: strings.Repeat("1", 40), BaseRef: "develop", Landed: true, ObservedAt: f.service.config.Clock()}
			attempt := changeColumnAttempt(t, f, &receipt, 100, true, true)
			var row eventstore.LandingReceipt
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				row, err = r.LandingReceipt(t.Context(), attempt.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if row.MergeSHA != strings.Repeat("3", 40) {
				t.Fatalf("typed merge ignored: %+v", row)
			}
			if _, err := eventstore.NewContent(f.service.runDetailStore()).LandingReceipt(t.Context(), row); !errors.Is(err, artifact.ErrIntegrity) {
				t.Fatalf("contradictory canonical receipt accepted: %v", err)
			}
		})
	}
}

func changeColumnAttempt(t *testing.T, f changeFixture, receipt *tracker.NativeLandingReceipt, local int64, writeReceipt, tamper bool) leasestore.Attempt {
	t.Helper()
	leaseID := hubapi.NewID("lease")
	lease := coordinatorActionLease(t, f.service, f.project.OrganizationID, f.issue.WorkItemID, leaseID)
	data := tracker.NativeRunData{AttemptID: hubapi.NewID("attempt"), RunID: hubapi.NewID("run"), LeaseID: tracker.LeaseID(leaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, Outcome: "succeeded", Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: local, Phase: "completed", HeartbeatAt: f.service.config.Clock(), Landing: receipt}}
	content := eventstore.NewContent(f.service.runDetailStore())
	ref, err := content.PreparePayload(t.Context(), f.project.OrganizationID, historyTestJSON(t, data))
	if err != nil {
		t.Fatal(err)
	}
	event, err := content.PrepareEvent(t.Context(), f.project.OrganizationID, historyTestJSON(t, tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateType: "issue", AggregateID: f.issue.WorkItemID, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "example-runner"}, RecordedAt: f.service.config.Clock(), Data: tracker.CollaborationData{Run: &data}}))
	if err != nil {
		t.Fatal(err)
	}
	var landing eventstore.LandingReceipt
	if writeReceipt {
		version, err := f.service.readChangeVersionRecord(t.Context(), f.project.OrganizationID, f.change.ID, receipt.VersionID)
		if err != nil {
			t.Fatal(err)
		}
		landing, err = content.PrepareLandingReceipt(t.Context(), eventstore.LandingReceipt{OrganizationID: f.project.OrganizationID, AttemptID: data.AttemptID, VersionID: receipt.VersionID, Repository: version.Repository, BaseRef: receipt.BaseRef}, *receipt)
		if err != nil {
			t.Fatal(err)
		}
		if tamper {
			landing.MergeSHA = strings.Repeat("3", 40)
		}
	}
	var attempt leasestore.Attempt
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		attempt, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: data.AttemptID, Fence: leasestore.Fence{LeaseID: leaseID, FencingToken: lease.FencingToken}, RunID: data.RunID, Sequence: 1, Status: "succeeded", LocalAttemptID: local, StartedAt: f.service.config.Clock(), PayloadObjectKey: ref.Key, PayloadReference: ref})
		if err != nil {
			return err
		}
		if _, err := w.AppendEvent(t.Context(), event); err != nil {
			return err
		}
		if writeReceipt {
			if _, err := w.CreateLandingReceipt(t.Context(), landing); err != nil {
				return err
			}
		}
		_, err = w.Release(t.Context(), leasestore.Fence{LeaseID: leaseID, FencingToken: lease.FencingToken}, "released")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return attempt
}

func changeColumnImportLandingReceipts(t *testing.T, f changeFixture, versions []tracker.ChangeVersion, attempts []leasestore.Attempt, receipts []tracker.NativeLandingReceipt) {
	t.Helper()
	content := eventstore.NewContent(f.service.runDetailStore())
	repository := f.service.config.TenantRepository.(*postgres.Repository)
	for i, version := range versions {
		var value tracker.NativeLandingReceipt
		if err := json.Unmarshal(historyTestJSON(t, receipts[i]), &value); err != nil || value.VersionID != version.ID {
			t.Fatal(errors.Join(eventstore.ErrInvalidRecord, err))
		}
		record, err := content.PrepareLandingReceipt(t.Context(), eventstore.LandingReceipt{OrganizationID: f.project.OrganizationID, AttemptID: attempts[i].ID, VersionID: version.ID, Repository: version.Repository, BaseRef: value.BaseRef}, value)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteRecords(t.Context(), f.project.OrganizationID, corestore.Background, func(w eventstore.Writer) error {
			_, err := w.CreateLandingReceipt(t.Context(), record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
