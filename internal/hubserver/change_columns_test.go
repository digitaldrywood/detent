package hubserver

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
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
