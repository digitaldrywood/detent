package coreimport_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/changerules"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport/coreimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeColumnsSelectEscapeOccurrence(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC)
	h := hubfixture.Open(t, hubfixture.Config{Clock: func() time.Time { return at }})
	authorization := hubauth.New(hubauth.Deps{Repository: h.Repository, AccessRepository: h.Repository, Organization: h.Primary, Now: h.Now, Classify: hubscope.OperatorAuthorizationError})
	quality := changerules.NewQualityEscapes(changerules.QualityEscapesDeps{
		Repository: h.Repository,
		AuthorizeAccessScope: func(ctx context.Context, r accessstore.Reader, scope hubscope.NativeScope, write bool) error {
			return hubapi.FromFeatureError(authorization.Authorize(ctx, r, hubauth.Input{Organization: scope.Organization, Project: scope.Project, Credential: hubscope.AuthCredential(scope.Credential)}, write))
		},
		RunDetailStoreFor: func(context.Context) attachment.DetailStore { return h.Config.RunDetails },
	})
	for _, pending := range []bool{false, true} {
		states := append(hubfixture.States(), tracker.NativeState{Name: "Merging"}, tracker.NativeState{Name: "Rework"})
		native := hubfixture.NewNativeWith(t, h, h.Primary, hubtest.ProjectOptions{Name: fmt.Sprintf("escape pending=%v", pending), States: states, RequireDependencies: true})
		var items []tracker.NativeIssue
		events := make(map[tracker.NativeWorkItemID][3]string)
		escape := func(id string, observed time.Time) *tracker.QualityEscape {
			return &tracker.QualityEscape{OccurrenceID: id, Kind: "reopened", Cause: "infrastructure", ObservedAt: observed, InstanceID: "example-instance", EvidenceReference: "history/" + id, EvidenceQuote: "Recorded workflow transition", BasisQuote: "Recorded instance failure"}
		}
		fixtureScope := qualityEscapeScope(t, native)
		escapeDiscussions := func(old, version tracker.ChangeVersion, ids [3]string) []tracker.ChangeDiscussion {
			return []tracker.ChangeDiscussion{{ID: "discussion-0", VersionID: version.ID, Actor: fixtureScope.Actor(), CreatedAt: at, Escape: escape(ids[0], at)}, {ID: "discussion-1", VersionID: version.ID, Actor: fixtureScope.Actor(), CreatedAt: at, Escape: escape("unrelated", at)}, {ID: "ordinary", VersionID: version.ID, Actor: fixtureScope.Actor(), CreatedAt: at}, {ID: "other version", VersionID: old.ID, Actor: fixtureScope.Actor(), CreatedAt: at, Escape: escape(ids[0], at)}}
		}
		historical := make(map[tracker.NativeWorkItemID][2]tracker.ChangeVersion)
		historicalChanges := make(map[tracker.NativeWorkItemID]tracker.ChangeRequest)
		if pending {
			descriptor := hubfixture.Policy()
			items, events = changeColumnHistoricalWorkflow(t, native, at, func(fixture *neutralimporttest.Fixture, ids map[tracker.NativeWorkItemID][3]string) {
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
			content := eventstore.NewContent(native.Hub.Config.RunDetails)
			repository := native.Hub.Repository
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
			items = []tracker.NativeIssue{native.Create(t, "first escape"), native.Create(t, "second escape")}
			for _, item := range items {
				events[item.WorkItemID] = changeColumnWorkflow(t, native, item, fixtureScope.Actor(), at)
			}
		}
		for _, item := range items {
			t.Run(fmt.Sprintf("%s/pending=%v", item.WorkItemID, pending), func(t *testing.T) {
				var f hubfixture.Change
				var old, version tracker.ChangeVersion
				if pending {
					old, version = historical[item.WorkItemID][0], historical[item.WorkItemID][1]
					f = hubfixture.Change{Native: native, Issue: item, Change: tracker.ChangeRequest{ID: old.ChangeID}}
				} else {
					f = changeColumnCreateChange(t, native, item)
					old = f.Publish(t, "old", "")
					version = f.Publish(t, "current", old.ID)
				}
				scope := fixtureScope
				ids := events[item.WorkItemID]
				content := eventstore.NewContent(f.Hub.Config.RunDetails)
				landing, err := content.PrepareVersionLanding(t.Context(), eventstore.VersionLanding{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item.WorkItemID, ChangeID: f.Change.ID, VersionID: version.ID}, tracker.ChangeLanding{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: strings.Repeat("a", 40), BaseRef: "develop", LandedAt: at})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Hub.Repository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { return w.RecordVersionLanding(t.Context(), landing) }); err != nil {
					t.Fatal(err)
				}
				discussions := escapeDiscussions(old, version, ids)
				wanted := discussions[0]
				tampered, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: f.Change.ID, Kind: "discussion"}, historyTestJSON(t, wanted))
				if err != nil {
					t.Fatal(err)
				}
				tampered.ReportEscape.OccurrenceID = "changed"
				if err := f.Hub.Repository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), tampered); return err }); err != nil {
					t.Fatal(err)
				}
				lookup := func(version, occurrence string) (eventstore.Evidence, error) {
					var row eventstore.Evidence
					err := f.Hub.Repository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
						reader, ok := r.(eventstore.QualityEscapeReader)
						if !ok {
							return hubapi.NativeRunEventStorageUnavailable()
						}
						var err error
						row, err = reader.QualityEscapeOccurrence(t.Context(), f.Change.ID, version, occurrence)
						return err
					})
					return row, err
				}
				if row, err := lookup(version.ID, ids[0]); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("typed occurrence ignored: row=%+v error=%v", row, err)
				}
				if pending {
					for i, discussion := range discussions {
						row, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: f.Change.ID, Sequence: int64(i + 2), VersionID: &discussion.VersionID, Kind: "discussion"}, historyTestJSON(t, discussion))
						if err != nil {
							t.Fatal(err)
						}
						if err := f.Hub.Repository.WriteRecords(t.Context(), scope.Organization, corestore.Background, func(w eventstore.Writer) error {
							_, err := w.AppendEvidence(t.Context(), row)
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					for _, discussion := range discussions {
						row, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: f.Change.ID, Kind: "discussion"}, historyTestJSON(t, discussion))
						if err != nil {
							t.Fatal(err)
						}
						if err := f.Hub.Repository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), row); return err }); err != nil {
							t.Fatal(err)
						}
					}
				}
				var changeRow eventstore.Change
				if err := h.Repository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					changeRow, err = r.Change(t.Context(), f.Change.ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				change, err := content.Change(t.Context(), changeRow)
				if err != nil {
					t.Fatal(err)
				}
				for i, id := range ids {
					_, err := quality.PrepareQualityEscapeTenant(t.Context(), scope, change, tracker.DiscussChange{VersionID: version.ID, Escape: escape(id, at.Add(time.Duration(i)*time.Second))}, at.Add(3*time.Second))
					if (err == nil) != (i == 0) {
						t.Fatalf("pending=%v event=%d reopened=%v error=%v", pending, i, err == nil, err)
					}
				}
				path := f.Base + "/work-items/" + string(item.WorkItemID) + "?view=recovery&projection=bounded"
				response := h.Request(t, http.MethodGet, path, f.Token, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var recovery tracker.NativeRecovery
				hubfixture.Decode(t, response, &recovery)
				if len(recovery.History) != 1 || recovery.History[0].ID != ids[1] {
					t.Fatalf("pending=%v landing exit=%+v", pending, recovery.History)
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

func changeColumnCreateChange(t *testing.T, f hubfixture.Native, issue tracker.NativeIssue) hubfixture.Change {
	t.Helper()
	hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", hubfixture.Policy())
	rules := tracker.ChangeReviewPolicy{PolicyID: hubfixture.Policy().ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}
	approved := f.Hub.Request(t, http.MethodPut, f.Base+"/change-review-policy", f.Hub.AdminToken(), tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "column-rules"}, Policy: rules})
	hubfixture.RequireStatus(t, approved, http.StatusOK)
	hubfixture.Decode(t, approved, &rules)
	path := f.Base + "/work-items/" + string(issue.WorkItemID) + "/changes"
	response := f.Hub.Request(t, http.MethodPost, path, f.Token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Column evidence"})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	hubfixture.Decode(t, response, &change)
	return hubfixture.Change{Native: f, Issue: issue, Change: change, Path: path + "/" + change.ID, Rules: rules}
}

func changeColumnWorkflow(t *testing.T, f hubfixture.Native, item tracker.NativeIssue, actor tracker.Actor, at time.Time) [3]string {
	t.Helper()
	var ids [3]string
	for i, data := range []tracker.CollaborationData{{FromState: "Done", ToState: "Todo"}, {FromState: "Merging", ToState: "Rework"}, {FromState: "Todo", ToState: "In Progress"}} {
		data.Revision = item.Revision
		value := tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, AggregateID: item.WorkItemID, AggregateType: "issue", Type: "workflow.transitioned", SchemaVersion: 1, Actor: actor, RecordedAt: at.Add(time.Duration(i) * time.Second), Data: data}
		row, err := eventstore.NewContent(f.Hub.Config.RunDetails).PrepareEvent(t.Context(), f.Project.OrganizationID, historyTestJSON(t, value))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Hub.Repository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), row); return err }); err != nil {
			t.Fatal(err)
		}
		ids[i] = value.ID
	}
	return ids
}

func changeColumnHistoricalWorkflow(t *testing.T, f hubfixture.Native, at time.Time, extend func(*neutralimporttest.Fixture, map[tracker.NativeWorkItemID][3]string)) ([]tracker.NativeIssue, map[tracker.NativeWorkItemID][3]string) {
	t.Helper()
	body := hubfixture.IssueBody() + "\n" + strings.Repeat("x", 8192)
	fixture := coreimporttest.WorkItems(t, os.DirFS("../../../.."), f.Project.OrganizationID, f.Project.ID, false, body)
	fixture.Add("workflow_states", map[string]any{"id": 2, "project_id": f.Project.ID, "detent_state": "In Progress"})
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
			fixture.Add("collaboration_events", map[string]any{"id": eventIDs[i], "organization_id": f.Project.OrganizationID, "project_id": f.Project.ID, "work_item_id": item, "sequence": i + 3, "type": "workflow.transitioned", "schema_version": 1, "actor_json": string(historyTestJSON(t, actor)), "data_json": string(historyTestJSON(t, data)), "event_revision": 2, "from_state": nil, "to_state": nil, "recorded_at": hubapi.FormatHubTime(at.Add(time.Duration(i) * time.Second))})
		}
		ids[item] = eventIDs
		for revision := 1; revision <= 2; revision++ {
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, WorkItemID: item, Number: 1 + index*2, Revision: tracker.Revision(revision), Profile: "native"}, Title: "Initial title", Body: body, State: "Todo", Labels: []string{"needs-review"}, Assignees: []string{"example-assignee"}, Actor: actor, CreatedAt: parseTestTime(t, "2026-10-01T00:00:00Z"), UpdatedAt: parseTestTime(t, "2026-10-01T00:00:00Z"), LastActivityAt: parseTestTime(t, "2026-10-01T00:00:00Z")}
			if revision == 2 {
				issue.Title, issue.State = "Updated title", "In Progress"
				issue.UpdatedAt, issue.LastActivityAt = at.Add(2*time.Second), at.Add(2*time.Second)
			}
			fixture.Add("collaboration_versions", map[string]any{"organization_id": f.Project.OrganizationID, "project_id": f.Project.ID, "work_item_id": item, "record_id": item, "revision": revision, "encoding": "snapshot", "record_json": string(historyTestJSON(t, issue))})
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
	source := coreimport.CoreWorkItemImportSource{BundlePath: fixture.Write(), OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, Objects: f.Hub.Config.RunDetails}
	if _, err := coreimport.ImportCoreWorkItems(t.Context(), f.Hub.Repository, source); err != nil {
		t.Fatal(err)
	}
	var items []tracker.NativeIssue
	for _, id := range []tracker.NativeWorkItemID{"wi_core_import", "wi_core_import_second"} {
		response := f.Hub.Request(t, http.MethodGet, f.Base+"/work-items/"+string(id), f.Token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		hubfixture.Decode(t, response, &issue)
		if issue.WorkItemID != id {
			t.Fatalf("imported issue = %s, want %s", issue.WorkItemID, id)
		}
		items = append(items, issue)
	}
	return items, ids
}

func qualityEscapeScope(t *testing.T, f hubfixture.Native) hubscope.NativeScope {
	t.Helper()
	credential, err := f.Credential(t, f.Token)
	if err != nil {
		t.Fatal(err)
	}
	return hubscope.NativeScope{
		Organization: f.Project.OrganizationID, Project: f.Project.ID,
		Credential: hubapi.APICredential{ID: credential.ID, Hash: credential.Hash, Scope: hubapi.APIScope(credential.Scope), NativeOnly: credential.NativeOnly},
	}
}
