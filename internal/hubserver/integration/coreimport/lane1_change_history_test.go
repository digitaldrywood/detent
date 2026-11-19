package coreimport_test

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeLegacyReadsServeOverflow(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	for _, family := range []string{"side data", "policy", "escape", "comments", "landing attempt", "reviews"} {
		t.Run(family, func(t *testing.T) {
			organization := tracker.OrganizationID("org_legacy_" + strings.ReplaceAll(family, " ", "_"))
			f := hubfixture.NewNamedChange(t, h, "Legacy "+family, organization)
			base := f.Publish(t, "version", "")
			count := 1
			if family == "side data" {
				count = hubrecords.ChangeLegacyReadLimit + 1
			}
			fixture, target, versions := historyRepairSource(t, f, base, count)
			version := versions[0]
			var attemptID string
			if family == "side data" {
				attemptID = historyRepairAttempt(t, target).ID
				for _, value := range versions {
					value.AttemptID = attemptID
					fixture.Set("change_versions", map[string]string{"id": value.ID}, map[string]any{"record_json": string(historyTestJSON(t, value))})
				}
			}
			if family == "reviews" {
				review := tracker.ChangeReview{ID: "older-review", VersionID: version.ID, ImplementationReview: &gate.ValidatorResult{}}
				fixture.Add("change_evidence", map[string]any{"sequence": 1, "change_id": target.Change.ID, "version_id": version.ID, "kind": "review", "source_key": "older-review", "record_json": string(historyTestJSON(t, review))})
			}
			for index := range hubrecords.ChangeLegacyReadLimit + 1 {
				id := fmt.Sprintf("legacy-%s-%03d", family, index)
				switch family {
				case "policy":
					change := target.Change
					change.ID, change.CurrentVersion = id, ""
					fixture.Add("change_requests", map[string]any{"id": id, "organization_id": target.Project.OrganizationID, "project_id": target.Project.ID, "work_item_id": target.Issue.WorkItemID, "record_json": string(historyTestJSON(t, change))})
					fixture.Add("change_issue_links", map[string]any{"change_id": id, "organization_id": target.Project.OrganizationID, "project_id": target.Project.ID, "work_item_id": target.Issue.WorkItemID})
				case "escape":
					discussion := tracker.ChangeDiscussion{ID: id, VersionID: version.ID, Body: "Historical discussion", Actor: tracker.Actor{Kind: "human", PrincipalID: "example-author"}, CreatedAt: target.Hub.Now()}
					fixture.Add("change_evidence", map[string]any{"sequence": index + 1, "change_id": target.Change.ID, "version_id": version.ID, "kind": "discussion", "source_key": nil, "record_json": string(historyTestJSON(t, discussion))})
				case "comments":
					historyRepairComment(t, target, id, int64(index+1))
				case "reviews":
					review := tracker.ChangeReview{ID: id, VersionID: version.ID, Decision: "commented"}
					fixture.Add("change_evidence", map[string]any{"sequence": index + 2, "change_id": target.Change.ID, "version_id": version.ID, "kind": "review", "source_key": nil, "record_json": string(historyTestJSON(t, review))})
				case "landing attempt":
					historyRepairAttempt(t, target)
				}
			}
			source := historyRepairImportSource(t, target, fixture)
			for range 2 {
				changes := int64(1)
				if family == "policy" {
					changes += hubrecords.ChangeLegacyReadLimit + 1
				}
				historyRepairImport(t, target, source, changes)
				if err := target.Hub.Repository.ReadTenant(t.Context(), target.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					switch family {
					case "reviews":
						reader, ok := r.(eventstore.ChangeRecoveryReader)
						if !ok {
							return hubapi.NativeRunEventStorageUnavailable()
						}
						rows, _, err := reader.RecoveryChangeEvidence(t.Context(), target.Change.ID, version.ID, math.MaxInt64, nil)
						if err != nil {
							return err
						}
						if len(rows) == 0 || rows[0].Sequence != 1 || rows[0].ReviewKind != "implementation" {
							t.Fatal("legacy overflow hid older implementation review")
						}
					case "side data":
						var after int64
						var total int
						for {
							rows, err := r.Versions(t.Context(), target.Change.ID, after, hubrecords.ChangeLegacyReadLimit)
							if err != nil {
								return err
							}
							if len(rows) == 0 {
								break
							}
							for _, row := range rows {
								if row.AttemptID != attemptID {
									t.Fatal("historical version lost its attempt link")
								}
							}
							total += len(rows)
							after = rows[len(rows)-1].Number
						}
						if total != hubrecords.ChangeLegacyReadLimit+1 {
							t.Fatalf("lost legacy attempts: %d", total)
						}
					case "policy":
						_, err := r.ProjectPolicyLanding(t.Context(), target.Project.ID, "owner/repository", strings.Repeat("a", 40), "develop")
						if !errors.Is(err, corestore.ErrNotFound) {
							t.Fatalf("unlanded historical changes gained policy authority: %v", err)
						}
					case "escape":
						reader, ok := r.(eventstore.QualityEscapeReader)
						if !ok {
							return hubapi.NativeRunEventStorageUnavailable()
						}
						_, err := reader.QualityEscapeOccurrence(t.Context(), target.Change.ID, version.ID, "missing")
						if !errors.Is(err, corestore.ErrNotFound) {
							t.Fatalf("historical discussions invented an escape occurrence: %v", err)
						}
					case "comments":
						rows, err := r.SelectComments(t.Context(), target.Project.ID, target.Issue.WorkItemID, math.MaxInt64, true, corestore.MaxPageSize)
						if err != nil {
							return err
						}
						if len(rows) != hubrecords.ChangeLegacyReadLimit+1 {
							t.Fatalf("lost historical comment records: %d", len(rows))
						}
					case "landing attempt":
						_, err := r.LatestLandingReceipt(t.Context(), target.Project.ID, target.Issue.WorkItemID)
						if !errors.Is(err, corestore.ErrNotFound) {
							t.Fatalf("historical attempts invented a landing receipt: %v", err)
						}
					}
					return nil
				}); err != nil {
					t.Fatalf("%s imported read failed: %v", family, err)
				}
				if family == "side data" {
					hubfixture.RequireStatus(t, target.Hub.Request(t, http.MethodGet, target.Base+"/work-items/"+string(target.Issue.WorkItemID), target.Token, nil), http.StatusOK)
				}
			}
		})
	}
}

func historyRepairSource(t *testing.T, f hubfixture.Change, base tracker.ChangeVersion, count int) (*neutralimporttest.Fixture, hubfixture.Change, []tracker.ChangeVersion) {
	t.Helper()
	change := f.Change
	change.ID, change.Revision, change.Landed = hubapi.NewID("change"), 1, nil
	versions := make([]tracker.ChangeVersion, count)
	for index := range versions {
		versions[index] = base
		versions[index].ID, versions[index].ChangeID, versions[index].Number = fmt.Sprintf("%s_version_%04d", change.ID, index), change.ID, int64(index+1)
	}
	change.CurrentVersion = versions[len(versions)-1].ID
	fixture := newCoreNeutralFixture(t, f.Project.OrganizationID, "change_requests", "change_issue_links", "change_versions", "change_evidence", "change_sources", "change_landing_receipts", "quality_landings", "change_viewed_files", "policy_revisions", "change_command_outputs", "change_finding_attributions", "change_content_objects", "attachments", "native_pr_publications", "release_deliveries")
	fixture.Add("policy_revisions", map[string]any{"scope": string(f.Project.OrganizationID) + "/" + string(f.Project.ID), "policy_id": base.PolicyID, "metadata_json": string(historyTestJSON(t, base.Policy))})
	fixture.Add("change_requests", map[string]any{"id": change.ID, "organization_id": f.Project.OrganizationID, "project_id": f.Project.ID, "work_item_id": f.Issue.WorkItemID, "record_json": string(historyTestJSON(t, change))})
	for _, item := range change.LinkedIssues {
		fixture.Add("change_issue_links", map[string]any{"change_id": change.ID, "organization_id": f.Project.OrganizationID, "project_id": f.Project.ID, "work_item_id": item})
	}
	for _, version := range versions {
		fixture.Add("change_versions", map[string]any{"id": version.ID, "change_id": change.ID, "number": version.Number, "record_json": string(historyTestJSON(t, version)), "policy_revision_id": version.PolicyID, "repository": version.Repository})
	}
	f.Change, f.Path = change, f.Base+"/work-items/"+string(f.Issue.WorkItemID)+"/changes/"+change.ID
	return fixture, f, versions
}

func historyRepairImportSource(t *testing.T, target hubfixture.Change, fixture *neutralimporttest.Fixture) coreimport.CoreChangeImportSource {
	t.Helper()
	objects := target.Hub.Config.RunDetails
	return coreimport.CoreChangeImportSource{BundlePath: fixture.Write(), OrganizationID: target.Project.OrganizationID, SourceObjects: objects, Objects: objects}
}

func historyRepairImport(t *testing.T, target hubfixture.Change, source coreimport.CoreChangeImportSource, count int64) {
	t.Helper()
	got, err := coreimport.ImportCoreChanges(t.Context(), target.Hub.Repository, source)
	if err != nil || got != count {
		t.Fatalf("historical repair import count=%d want=%d error=%v", got, count, err)
	}
}

func historyRepairAttempt(t *testing.T, f hubfixture.Change) leasestore.Attempt {
	t.Helper()
	leaseID := hubapi.NewID("lease")
	var lease leasestore.Lease
	writeLegacyChange(t, f, func(w tenantstore.Writer) error {
		item, err := w.WorkItem(t.Context(), f.Project.ID, f.Issue.WorkItemID)
		if err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: f.Project.OrganizationID, ID: "archive-machine", Hostname: "archive-host", Capacity: 1, Version: "test"}); err != nil {
			return err
		}
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "archive-machine", SessionID: leaseID + "-session", LeaseID: leaseID, TTL: time.Hour})
		return err
	})
	data := tracker.NativeRunData{AttemptID: hubapi.NewID("attempt"), RunID: hubapi.NewID("run"), LeaseID: tracker.LeaseID(leaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, Outcome: "succeeded"}
	content := eventstore.NewContent(f.Hub.Config.RunDetails)
	ref, err := content.PreparePayload(t.Context(), f.Project.OrganizationID, historyTestJSON(t, data))
	if err != nil {
		t.Fatal(err)
	}
	event, err := content.PrepareEvent(t.Context(), f.Project.OrganizationID, historyTestJSON(t, tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, AggregateType: "issue", AggregateID: f.Issue.WorkItemID, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "example-runner"}, RecordedAt: f.Hub.Now(), Data: tracker.CollaborationData{Run: &data}}))
	if err != nil {
		t.Fatal(err)
	}
	var attempt leasestore.Attempt
	writeLegacyChange(t, f, func(w tenantstore.Writer) error {
		var err error
		attempt, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: data.AttemptID, Fence: leasestore.Fence{LeaseID: leaseID, FencingToken: lease.FencingToken}, RunID: data.RunID, Sequence: 1, Status: "succeeded", StartedAt: f.Hub.Now(), PayloadObjectKey: ref.Key, PayloadReference: ref})
		if err != nil {
			return err
		}
		if _, err := w.AppendEvent(t.Context(), event); err != nil {
			return err
		}
		_, err = w.Release(t.Context(), leasestore.Fence{LeaseID: leaseID, FencingToken: lease.FencingToken}, "released")
		return err
	})
	return attempt
}

func historyRepairComment(t *testing.T, f hubfixture.Change, id string, sequence int64) {
	t.Helper()
	at := f.Hub.Now()
	comment := tracker.NativeComment{OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, WorkItemID: f.Issue.WorkItemID, ID: id, Revision: 1, Sequence: sequence, Actor: tracker.Actor{Kind: "runner", PrincipalID: "example-runner"}, Body: "Historical comment", CreatedAt: at, UpdatedAt: at}
	content := eventstore.NewContent(f.Hub.Config.RunDetails)
	version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, WorkItemID: f.Issue.WorkItemID, RecordID: id, Revision: 1}, historyTestJSON(t, comment))
	if err != nil {
		t.Fatal(err)
	}
	fields, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.Project.OrganizationID}, historyTestJSON(t, comment.Body))
	if err != nil {
		t.Fatal(err)
	}
	writeLegacyChange(t, f, func(w tenantstore.Writer) error {
		if _, err := w.CreateComment(t.Context(), corestore.Comment{OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, WorkItemID: f.Issue.WorkItemID, ID: id, Revision: 1, Sequence: sequence, BodyObjectKey: fields.PayloadObjectKey, ActorKind: comment.Actor.Kind, ActorPrincipalID: comment.Actor.PrincipalID, CreatedAt: at, UpdatedAt: at}); err != nil {
			return err
		}
		_, err := w.CreateCollaborationVersion(t.Context(), version)
		return err
	})
}

func writeLegacyChange(t *testing.T, f hubfixture.Change, write func(tenantstore.Writer) error) {
	t.Helper()
	if err := f.Hub.Repository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, write); err != nil {
		t.Fatal(err)
	}
}
