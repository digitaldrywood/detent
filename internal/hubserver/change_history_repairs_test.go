package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeLegacyReadsServeOverflow(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"attempts", "side data", "policy", "escape", "comments", "landing attempt", "reviews"} {
		t.Run(family, func(t *testing.T) {
			f := newChangeFixture(t, nil)
			base := f.publish(t, "version", "")
			count := 1
			if family == "attempts" || family == "side data" {
				count = changeLegacyReadLimit + 1
			}
			fixture, target, versions := historyRepairSource(t, f, base, count)
			version := versions[0]
			if family == "attempts" || family == "side data" {
				attempt := historyRepairAttempt(t, target, tracker.NativeRunData{})
				for _, value := range versions {
					value.AttemptID = attempt.ID
					fixture.set("change_versions", map[string]string{"id": value.ID}, map[string]any{"record_json": string(historyTestJSON(t, value))})
				}
			}
			if family == "reviews" {
				review := tracker.ChangeReview{ID: "older-review", VersionID: version.ID, ImplementationReview: &gate.ValidatorResult{}}
				fixture.add("change_evidence", map[string]any{"sequence": 1, "change_id": target.change.ID, "version_id": version.ID, "kind": "review", "source_key": "older-review", "record_json": string(historyTestJSON(t, review))})
			}
			for index := range changeLegacyReadLimit + 1 {
				id := fmt.Sprintf("legacy-%s-%03d", family, index)
				switch family {
				case "policy":
					change := target.change
					change.ID, change.CurrentVersion = id, ""
					fixture.add("change_requests", map[string]any{"id": id, "organization_id": target.project.OrganizationID, "project_id": target.project.ID, "work_item_id": target.issue.WorkItemID, "record_json": string(historyTestJSON(t, change))})
					fixture.add("change_issue_links", map[string]any{"change_id": id, "organization_id": target.project.OrganizationID, "project_id": target.project.ID, "work_item_id": target.issue.WorkItemID})
				case "escape":
					discussion := tracker.ChangeDiscussion{ID: id, VersionID: version.ID, Body: "Historical discussion", Actor: tracker.Actor{Kind: "human", PrincipalID: "example-author"}, CreatedAt: target.service.config.now()}
					fixture.add("change_evidence", map[string]any{"sequence": index + 1, "change_id": target.change.ID, "version_id": version.ID, "kind": "discussion", "source_key": nil, "record_json": string(historyTestJSON(t, discussion))})
				case "comments":
					historyRepairComment(t, target, id, int64(index+1))
				case "reviews":
					review := tracker.ChangeReview{ID: id, VersionID: version.ID, Decision: "commented"}
					fixture.add("change_evidence", map[string]any{"sequence": index + 2, "change_id": target.change.ID, "version_id": version.ID, "kind": "review", "source_key": nil, "record_json": string(historyTestJSON(t, review))})
				case "landing attempt":
					historyRepairAttempt(t, target, tracker.NativeRunData{})
				}
			}
			source := historyRepairImportSource(t, target, fixture)
			for range 2 {
				changes := int64(1)
				if family == "policy" {
					changes += changeLegacyReadLimit + 1
				}
				historyRepairImport(t, target, source, changes)
				if err := target.service.config.TenantRepository.ReadTenant(t.Context(), target.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					switch family {
					case "reviews":
						reader, ok := r.(eventstore.ChangeRecoveryReader)
						if !ok {
							return nativeRunEventStorageUnavailable()
						}
						rows, _, err := reader.RecoveryChangeEvidence(t.Context(), target.change.ID, version.ID, math.MaxInt64, nil)
						if err != nil {
							return err
						}
						if len(rows) != 33 {
							t.Fatalf("legacy overflow hid older implementation review: selected=%d", len(rows))
						}
						if rows[0].ReviewKind != "implementation" {
							t.Fatal("legacy overflow hid older implementation review")
						}
					case "attempts", "side data":
						var after int64
						var total int
						for {
							rows, err := r.Versions(t.Context(), target.change.ID, after, changeLegacyReadLimit)
							if err != nil {
								return err
							}
							if len(rows) == 0 {
								break
							}
							for _, row := range rows {
								if row.AttemptID == "" {
									t.Fatal("historical version lost its attempt link")
								}
							}
							total += len(rows)
							after = rows[len(rows)-1].Number
						}
						if total != changeLegacyReadLimit+1 {
							t.Fatalf("lost legacy attempts: %d", total)
						}
					case "policy":
						_, err := r.ProjectPolicyLanding(t.Context(), target.project.ID, "owner/repository", strings.Repeat("a", 40), "develop")
						if !errors.Is(err, corestore.ErrNotFound) {
							t.Fatalf("unlanded historical changes gained policy authority: %v", err)
						}
					case "escape":
						reader, ok := r.(eventstore.QualityEscapeReader)
						if !ok {
							return nativeRunEventStorageUnavailable()
						}
						_, err := reader.QualityEscapeOccurrence(t.Context(), target.change.ID, version.ID, "missing")
						if !errors.Is(err, corestore.ErrNotFound) {
							t.Fatalf("historical discussions invented an escape occurrence: %v", err)
						}
					case "comments":
						rows, err := r.SelectComments(t.Context(), target.project.ID, target.issue.WorkItemID, math.MaxInt64, true, corestore.MaxPageSize)
						if err != nil {
							return err
						}
						if len(rows) != changeLegacyReadLimit+1 {
							t.Fatalf("lost historical comment records: %d", len(rows))
						}
					case "landing attempt":
						_, err := r.LatestLandingReceipt(t.Context(), target.project.ID, target.issue.WorkItemID)
						if !errors.Is(err, corestore.ErrNotFound) {
							t.Fatalf("historical attempts invented a landing receipt: %v", err)
						}
					}
					return nil
				}); err != nil {
					t.Fatalf("%s imported read failed: %v", family, err)
				}
				if family == "side data" {
					requireNativeStatus(t, performHubAPIRequest(t, target.service, http.MethodGet, target.base+"/work-items/"+string(target.issue.WorkItemID), target.token, nil), http.StatusOK)
				}
			}
		})
	}
}

func TestRecoveryUnknownHistoryBudgetPreservesOlderEvidence(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	for _, f := range []changeFixture{first, newChangeFixture(t, first.service, first.project.OrganizationID+"_second")} {
		version := f.publish(t, "version", "")
		for _, family := range []string{"check", "landing exit"} {
			t.Run(f.change.ID+"/"+family, func(t *testing.T) {
				if family == "landing exit" {
					actor := tracker.Actor{Kind: "runner", PrincipalID: "example-runner"}
					appendTestHealthEventAs(t, f.nativeFixture, actor, f.issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Merging", ToState: "Rework", Reason: "landing_refused"}, f.service.config.now())
					for range changeLegacyReadLimit - 1 {
						appendTestHealthEventAs(t, f.nativeFixture, actor, f.issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}, f.service.config.now())
					}
					read := func(message string) {
						t.Helper()
						value, err := f.service.readNativeRecoveryTenant(t.Context(), analyticsTestScope(t, f.nativeFixture), string(f.issue.WorkItemID))
						if err != nil || len(value.History) != 1 || value.History[0].Data.FromState != "Merging" || value.History[0].Data.Reason != "landing_refused" {
							t.Fatalf("%s: history=%+v error=%v", message, value.History, err)
						}
					}
					read("boundary hid older evidence")
					appendTestHealthEventAs(t, f.nativeFixture, actor, f.issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}, f.service.config.now())
					read("legacy overflow hid older evidence")
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						rows, err := r.EventsBefore(t.Context(), f.project.ID, f.issue.WorkItemID, math.MaxInt64, corestore.MaxPageSize)
						if err != nil {
							return err
						}
						for _, row := range rows {
							if row.Type == "workflow.transitioned" && row.FromState == "" {
								t.Fatal("canonical workflow event lost its typed from-state projection")
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					read("converted history hid older evidence")
					return
				}
				fixture, target, versions := historyRepairSource(t, f, version, 1)
				version := versions[0]
				for index := range changeLegacyReadLimit + 1 {
					id, conclusion := fmt.Sprintf("other-%03d", index), ""
					if index == 0 {
						id, conclusion = "wanted", "failure"
					}
					check := tracker.ChangeCheck{VersionID: version.ID, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: id, Conclusion: conclusion}}
					fixture.add("change_evidence", map[string]any{"sequence": index + 1, "change_id": target.change.ID, "version_id": version.ID, "kind": "check", "source_key": id, "record_json": string(historyTestJSON(t, check))})
				}
				source := historyRepairImportSource(t, target, fixture)
				interrupted := errors.New("historical evidence import interrupted")
				pause := &historyRepairPauseDetails{DetailStore: source.Objects, marker: []byte(`"check_run_id":"other-100"`), failure: interrupted}
				source.Objects = pause
				if _, err := ImportCoreChanges(t.Context(), target.service.config.TenantRepository.(*postgres.Repository), source); !errors.Is(err, interrupted) || !pause.fired {
					t.Fatalf("historical import did not pause after the boundary: %v", err)
				}
				read := func(through int64, message string) {
					t.Helper()
					var rows []eventstore.Evidence
					if err := target.service.config.TenantRepository.ReadTenant(t.Context(), target.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						reader, ok := r.(eventstore.ChangeRecoveryReader)
						if !ok {
							return nativeRunEventStorageUnavailable()
						}
						var err error
						rows, _, err = reader.RecoveryChangeEvidence(t.Context(), target.change.ID, version.ID, through, []string{"wanted"})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if len(rows) != 1 {
						t.Fatalf("%s: selected=%d", message, len(rows))
					}
					raw, err := eventstore.NewContent(target.service.runDetailStore()).Evidence(t.Context(), rows[0])
					if err != nil {
						t.Fatal(err)
					}
					var check tracker.ChangeCheck
					if json.Unmarshal(raw, &check) != nil || check.CheckRunID != "wanted" || check.Conclusion != "failure" {
						t.Fatalf("%s: restored=%+v", message, check)
					}
				}
				source.Objects = source.SourceObjects
				read(changeLegacyReadLimit, "boundary hid older evidence")
				historyRepairImport(t, target, source, 1)
				read(changeLegacyReadLimit+1, "legacy overflow hid older evidence")
				historyRepairImport(t, target, source, 1)
				read(changeLegacyReadLimit+1, "converted history hid older evidence")
			})
		}
	}
}

func TestValidationCandidatesKeepHistoricalMatchBeyondPage(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	base := f.publish(t, "old", "")
	head := strings.Repeat("a", 40)
	fixture, target, versions, attempts := historyRepairValidationSource(t, f, base, head)
	source := historyRepairImportSource(t, target, fixture)
	scope := analyticsTestScope(t, target.nativeFixture)
	repository := validationRepository(base.Repository)
	fingerprint := artifact.Digest([]byte(string(target.issue.WorkItemID)))
	scheduled := gate.ScheduledEvidence{Schema: 1, Repository: repository, OccurrenceKey: fingerprint, RunID: "123", RunAttempt: "1", JobID: "456", JobName: "Check", RunURL: "https://github.com/" + repository + "/actions/runs/123/attempts/1", JobURL: "https://github.com/" + repository + "/actions/jobs/456", HeadSHA: head, Conclusion: "failure"}
	body := issueorigin.Stamp("Scheduled check failed"+scheduled.Stamp(), issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/" + repository + "/actions/runs/123", Fingerprint: fingerprint})
	requireNativeStatus(t, performHubAPIRequest(t, target.service, http.MethodPost, target.base+"/work-items/"+string(target.issue.WorkItemID)+"/comments", target.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "historical-validation-source"}, Body: body}), http.StatusOK)
	for pass := range 2 {
		historyRepairImport(t, target, source, 1)
		if err := target.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			for index, attempt := range attempts {
				row, err := r.LandingReceipt(t.Context(), attempt.ID)
				if err != nil {
					return err
				}
				want := strings.Repeat("b", 40)
				if index == 0 {
					want = head
				}
				if row.MergeSHA != want || row.VersionID != versions[index].ID || row.AttemptID != attempt.ID {
					t.Fatal("historical receipt import lost typed merge or attempt identity")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		issue, err := coordinatorActionIssue(t.Context(), target.service, scope.organization, scope.project, target.issue.WorkItemID)
		if err != nil {
			t.Fatal(err)
		}
		audit, err := target.service.readValidationAuditRecord(t.Context(), scope, issue, nil)
		if err != nil || audit == nil || !audit.Partial || len(audit.Occurrences) != 1 {
			t.Fatalf("historical validation source unavailable: audit=%+v error=%v", audit, err)
		}
		rows := audit.Occurrences[0].Comparisons
		noReceipt := versions[len(versions)-1]
		if len(rows) != 20 || rows[0].VersionID != versions[0].ID || rows[0].IntegratedSource != "same_commit" || slices.ContainsFunc(rows, func(row tracker.ValidationComparison) bool { return row.VersionID == noReceipt.ID }) {
			t.Fatalf("lost historical match beyond 20 nonmatches: rows=%+v partial=%v", rows, audit.Partial)
		}
		if pass != 0 && (rows[0].VersionID != versions[0].ID || rows[0].IntegratedSource != "same_commit") {
			t.Fatal("legacy receipts hid older match")
		}
	}
}

func TestActivityTypedAndHistoricalAttemptLinks(t *testing.T) {
	f := newActivityPostgresFixture(t, "org_activity_links")
	content := eventstore.NewContent(f.service.runDetailStore())
	store := func(issue corestore.WorkItem, id string, at time.Time, run tracker.NativeRunData, assigned bool) {
		t.Helper()
		f.fence++
		start, end := at.Add(-time.Minute), at
		lease := leasestore.Lease{OrganizationID: f.organization, LeaseID: "lease_" + id, FencingToken: f.fence, IssueID: issue.ID, MachineID: "machine_browser_preview", SessionID: "lease-session-" + id, WorkItemRevision: issue.Revision, AcquiredAt: start, RenewedAt: start, ExpiresAt: at.Add(time.Hour), ReleasedAt: &end}
		if err := f.repository.ImportCoreLease(t.Context(), postgres.CoreLeaseImport{Lease: lease, Receipt: f.receipt(t, "core-lease", lease.LeaseID, lease)}); err != nil {
			t.Fatal(err)
		}
		if assigned {
			assignment := runnerstore.LeaseAssignment{OrganizationID: f.organization, LeaseID: lease.LeaseID, RunnerID: "runner_browser_preview", AssignedAt: start}
			if err := f.repository.ImportCoreRunnerLeaseAssignment(t.Context(), postgres.CoreRunnerLeaseAssignmentImport{Assignment: assignment, Receipt: f.receipt(t, "core-runner-lease-assignment", lease.LeaseID, assignment)}); err != nil {
				t.Fatal(err)
			}
		}
		run.AttemptID, run.RunID, run.LeaseID, run.FencingToken, run.Sequence, run.Outcome = id, id, tracker.LeaseID(lease.LeaseID), tracker.FencingToken(lease.FencingToken), 1, "succeeded"
		raw, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := f.service.runDetailStore().StoreDetail(t.Context(), string(f.organization), raw)
		if err != nil {
			t.Fatal(err)
		}
		attempt := leasestore.Attempt{OrganizationID: f.organization, ID: id, IssueID: issue.ID, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, RunID: id, Sequence: 1, Status: "succeeded", WorkItemRevision: issue.Revision, PayloadObjectKey: ref.Key, StartedAt: start, UpdatedAt: at}
		prepared := postgres.CoreAttemptImport{Attempt: attempt}
		if run.Handoff != nil {
			raw, err := json.Marshal(run.Handoff)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := f.service.runDetailStore().StoreDetail(t.Context(), string(f.organization), raw)
			if err != nil {
				t.Fatal(err)
			}
			prepared.Attempt.CheckpointObjectKey, prepared.CheckpointReference = &checkpoint.Key, checkpoint
		}
		prepared.Receipt = f.receipt(t, "core-attempt", id, prepared.Attempt)
		if err := f.repository.ImportCoreAttempt(t.Context(), prepared); err != nil {
			t.Fatal(err)
		}
		raw, err = json.Marshal(tracker.CollaborationEvent{ID: "event_" + id, OrganizationID: f.organization, ProjectID: issue.ProjectID, AggregateID: issue.NativeID, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "machine", PrincipalID: "machine_browser_preview"}, RecordedAt: at, Data: tracker.CollaborationData{Run: &run}})
		if err != nil {
			t.Fatal(err)
		}
		event, err := content.PrepareEvent(t.Context(), f.organization, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.repository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
			t.Fatal(err)
		}
	}
	for entity := 0; entity < 2; entity++ {
		project := f.createProject(t, fmt.Sprintf("Activity links %d", entity))
		issue := f.issue(t, project, fmt.Sprintf("Activity links %d", entity))
		changeID, versionID := fmt.Sprintf("activity_change_%d", entity), fmt.Sprintf("activity_version_%d", entity)
		if err := f.repository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
			policy, err := w.ProjectPolicy(t.Context(), issue.ProjectID)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(tracker.ChangeRequest{ID: changeID, OrganizationID: f.organization, ProjectID: issue.ProjectID, WorkItemID: issue.NativeID, Title: "Activity change", Revision: 1, CreatedAt: f.now.Add(-time.Hour), UpdatedAt: f.now.Add(-time.Hour)})
			if err != nil {
				return err
			}
			prepared, err := content.PrepareChange(t.Context(), f.organization, raw)
			if err != nil {
				return err
			}
			change, err := w.CreateChange(t.Context(), prepared)
			if err != nil {
				return err
			}
			raw, err = json.Marshal(tracker.ChangeVersion{ID: versionID, ChangeID: changeID, Number: 1, CreatedAt: f.now.Add(-time.Hour), Actor: tracker.Actor{Kind: "human", PrincipalID: "user_browser_owner"}, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), PolicyID: policy.PolicyID, Repository: "example-org/example-project", External: &tracker.ChangeExternalReference{Provider: "github", ID: "42", URL: "https://example.test/pull/42"}}})
			if err != nil {
				return err
			}
			version, err := content.PrepareVersion(t.Context(), f.organization, raw)
			if err != nil {
				return err
			}
			if _, err := w.CreateVersion(t.Context(), version); err != nil {
				return err
			}
			change.CurrentVersionID = &version.ID
			if _, err := w.UpdateChange(t.Context(), change); err != nil {
				return err
			}
			return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: f.organization, ProjectID: issue.ProjectID, WorkItemID: issue.NativeID, ChangeID: changeID, CreatedAt: f.now.Add(-time.Hour)})
		}); err != nil {
			t.Fatal(err)
		}
		for sourceIndex, source := range []string{"finalization", "checkpoint", "latest Change"} {
			for _, assigned := range []bool{false, true} {
				name := fmt.Sprintf("entity_%d/%s/assigned_%t", entity, source, assigned)
				t.Run(name, func(t *testing.T) {
					index := sourceIndex * 2
					if assigned {
						index++
					}
					at := f.now.Add(-30*time.Minute + time.Duration(index)*3*time.Minute)
					id := fmt.Sprintf("activity_links_%d_%d", entity, index)
					run := tracker.NativeRunData{RunnerID: "runner_browser_preview", SessionID: "recorded-session", Identity: &tracker.NativeExecutionIdentity{Role: "code"}, Runtime: &tracker.NativeRuntimeObservation{Phase: "implementation", PhasesDropped: 2, Phases: []tracker.NativePhase{{Name: "implementation", StartedAt: at.Add(-time.Minute), FinishedAt: at}}}}
					if assigned {
						run.RunnerID = "payload-runner"
						run.Runtime.Identity = agentidentity.Configured("codex", "codex", "", "code", "example", "openai", "high", "", at)
					}
					wantURL := "https://example.test/pull/42"
					switch source {
					case "finalization":
						wantURL = "https://example.test/pull/99"
						if assigned {
							wantURL = "https://example.test/pull/77"
						}
						run.Finalization = &tracker.NativeFinalization{ChangeID: changeID, Publication: &tracker.NativePRPublication{External: tracker.ChangeExternalReference{URL: wantURL}}}
					case "checkpoint":
						run.Handoff = &tracker.NativeCheckpoint{Change: &tracker.NativeChangeReference{ChangeID: changeID}}
					}
					store(issue, id, at, run, assigned)
					runnerFilter := ""
					if assigned {
						runnerFilter = "&runner_id=runner_browser_preview"
					}
					path := "/api/v2/organizations/" + string(f.organization) + "/activity?project_id=" + project + "&limit=5&from=" + at.Add(-2*time.Minute).Format(time.RFC3339Nano) + "&to=" + at.Add(time.Second).Format(time.RFC3339Nano) + runnerFilter
					response := f.page(t, "owner", path)
					browserHostedStatus(t, response, http.StatusOK)
					var report operatortool.ActivityReport
					usageDecode(t, response, &report)
					if len(report.Finished) != 1 {
						t.Fatalf("source=%s assigned=%t activity=%+v", source, assigned, report)
					}
					got := report.Finished[0]
					if got.ChangeID != changeID || got.PullRequestURL != wantURL || got.RunnerID != "runner_browser_preview" || got.SessionID != "recorded-session" || got.Phase != "implementation" || got.Stage != "code" || !got.Partial || got.StageDurationSeconds == nil || *got.StageDurationSeconds != 60 {
						t.Fatalf("source=%s assigned=%t lost rendered evidence: %+v", source, assigned, got)
					}
				})
			}
		}
		newerChangeID, newerVersionID := fmt.Sprintf("activity_newer_change_%d", entity), fmt.Sprintf("activity_newer_version_%d", entity)
		if err := f.repository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
			policy, err := w.ProjectPolicy(t.Context(), issue.ProjectID)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(tracker.ChangeRequest{ID: newerChangeID, OrganizationID: f.organization, ProjectID: issue.ProjectID, WorkItemID: issue.NativeID, Title: "Newer activity change", Revision: 1, CreatedAt: f.now.Add(-30 * time.Minute), UpdatedAt: f.now.Add(-30 * time.Minute)})
			if err != nil {
				return err
			}
			prepared, err := content.PrepareChange(t.Context(), f.organization, raw)
			if err != nil {
				return err
			}
			change, err := w.CreateChange(t.Context(), prepared)
			if err != nil {
				return err
			}
			raw, err = json.Marshal(tracker.ChangeVersion{ID: newerVersionID, ChangeID: newerChangeID, Number: 1, CreatedAt: f.now.Add(-30 * time.Minute), Actor: tracker.Actor{Kind: "human", PrincipalID: "user_browser_owner"}, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), PolicyID: policy.PolicyID, Repository: "example-org/example-project", External: &tracker.ChangeExternalReference{Provider: "github", ID: "43", URL: "https://example.test/pull/43"}}})
			if err != nil {
				return err
			}
			version, err := content.PrepareVersion(t.Context(), f.organization, raw)
			if err != nil {
				return err
			}
			if _, err := w.CreateVersion(t.Context(), version); err != nil {
				return err
			}
			change.CurrentVersionID = &version.ID
			if _, err := w.UpdateChange(t.Context(), change); err != nil {
				return err
			}
			return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: f.organization, ProjectID: issue.ProjectID, WorkItemID: issue.NativeID, ChangeID: newerChangeID, CreatedAt: f.now.Add(-30 * time.Minute)})
		}); err != nil {
			t.Fatal(err)
		}
		for sourceIndex, source := range []string{"finalization", "checkpoint"} {
			t.Run(fmt.Sprintf("entity_%d/%s_before_newer_link", entity, source), func(t *testing.T) {
				at := f.now.Add(-9*time.Minute + time.Duration(sourceIndex)*3*time.Minute)
				id := fmt.Sprintf("activity_links_precedence_%d_%d", entity, sourceIndex)
				run := tracker.NativeRunData{RunnerID: "payload-runner", SessionID: "recorded-session", Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Configured("codex", "codex", "", "code", "example", "openai", "high", "", at), Phase: "implementation", PhasesDropped: 2, Phases: []tracker.NativePhase{{Name: "implementation", StartedAt: at.Add(-time.Minute), FinishedAt: at}}}}
				wantURL := ""
				if source == "finalization" {
					wantURL = "https://example.test/pull/99"
					run.Finalization = &tracker.NativeFinalization{ChangeID: changeID, Publication: &tracker.NativePRPublication{External: tracker.ChangeExternalReference{URL: wantURL}}}
				} else {
					run.Handoff = &tracker.NativeCheckpoint{Change: &tracker.NativeChangeReference{ChangeID: changeID}}
				}
				store(issue, id, at, run, true)
				path := "/api/v2/organizations/" + string(f.organization) + "/activity?project_id=" + project + "&limit=5&runner_id=runner_browser_preview&from=" + at.Add(-2*time.Minute).Format(time.RFC3339Nano) + "&to=" + at.Add(time.Second).Format(time.RFC3339Nano)
				response := f.page(t, "owner", path)
				browserHostedStatus(t, response, http.StatusOK)
				var report operatortool.ActivityReport
				usageDecode(t, response, &report)
				if len(report.Finished) != 1 {
					t.Fatalf("source=%s newer linked Change activity=%+v", source, report)
				}
				got := report.Finished[0]
				if got.ChangeID != changeID || got.PullRequestURL != wantURL || got.RunnerID != "runner_browser_preview" || got.SessionID != "recorded-session" || got.Phase != "implementation" || got.Stage != "code" || !got.Partial || got.StageDurationSeconds == nil || *got.StageDurationSeconds != 60 {
					t.Fatalf("source=%s replaced named Change or leaked unrelated URL: %+v", source, got)
				}
			})
		}
		for index := 0; index <= changeLegacyReadLimit; index++ {
			f.storeAttempt(t, issue, fmt.Sprintf("other_runner_%d_%d", entity, index), "succeeded", f.now.Add(-2*time.Minute), f.now.Add(-time.Minute), nil, false)
		}
		response := f.page(t, "owner", "/api/v2/organizations/"+string(f.organization)+"/activity?project_id="+project+"&limit=5&runner_id=missing-runner")
		browserHostedStatus(t, response, http.StatusOK)
		var report operatortool.ActivityReport
		usageDecode(t, response, &report)
		if len(report.Finished) != 0 || len(report.Running) != 0 {
			t.Fatalf("activity runner filter failed after overflow: %+v", report)
		}
	}
}

func TestLegacyClaimReviewsRetainGateAuthority(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	version := f.publish(t, "initial", "")
	fixture, target, versions := historyRepairSource(t, f, version, 1)
	version = versions[0]
	reviews := []tracker.ChangeReview{
		{ID: "human-approval", Actor: tracker.Actor{PrincipalID: "first-reviewer"}, Decision: "approved"},
		{ID: "old-implementation", ImplementationReview: &gate.ValidatorResult{Verdict: "pass"}, Decision: "approved"},
		{ID: "old-validator", Validator: &gate.ValidatorResult{Verdict: "pass"}, Decision: "approved"},
		{ID: "source-rejection", SecurityAudit: &tracker.SecurityAuditReview{}, Decision: "changes_requested"},
		{ID: "landing-rejection", SecurityAudit: &tracker.SecurityAuditReview{Landing: true}, Decision: "changes_requested"},
	}
	for i := range claimEvidenceLimit + 1 {
		reviews = append(reviews, tracker.ChangeReview{ID: fmt.Sprintf("comment-%d", i), Actor: tracker.Actor{PrincipalID: "first-reviewer"}, Decision: "commented"})
	}
	reviews = append(reviews,
		tracker.ChangeReview{ID: "human-refusal", Actor: tracker.Actor{PrincipalID: "second-reviewer"}, Decision: "changes_requested"},
		tracker.ChangeReview{ID: "implementation", ImplementationReview: &gate.ValidatorResult{Verdict: "fail"}, Decision: "changes_requested"},
		tracker.ChangeReview{ID: "validator", Validator: &gate.ValidatorResult{Verdict: "pass"}, Decision: "approved"},
		tracker.ChangeReview{ID: "source-approval", SecurityAudit: &tracker.SecurityAuditReview{}, Decision: "approved"},
		tracker.ChangeReview{ID: "landing-approval", SecurityAudit: &tracker.SecurityAuditReview{Landing: true}, Decision: "approved"},
	)
	for index, review := range reviews {
		review.VersionID = version.ID
		fixture.add("change_evidence", map[string]any{"sequence": index + 1, "change_id": target.change.ID, "version_id": version.ID, "kind": "review", "source_key": nil, "record_json": string(historyTestJSON(t, review)), "payload_object_key": ""})
	}
	source := historyRepairImportSource(t, target, fixture)
	for range 2 {
		historyRepairImport(t, target, source, 1)
		var rows []eventstore.Evidence
		if err := target.service.config.TenantRepository.ReadTenant(t.Context(), target.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(tenantstore.ClaimChangeRowsReader)
			if !ok {
				return nativeRunEventStorageUnavailable()
			}
			facts, err := reader.ClaimChangeRows(t.Context(), target.project.ID, []string{target.change.ID})
			rows = facts[target.change.ID].Evidence
			return err
		}); err != nil {
			t.Fatal(err)
		}
		selected := make([]tracker.ChangeReview, 0, len(rows))
		for _, row := range rows {
			if row.Kind != "review" {
				continue
			}
			raw, err := eventstore.NewContent(target.service.runDetailStore()).Evidence(t.Context(), row)
			if err != nil {
				t.Fatal(err)
			}
			var review tracker.ChangeReview
			if err := json.Unmarshal(raw, &review); err != nil {
				t.Fatal(err)
			}
			selected = append(selected, review)
		}
		ids := make([]string, 0, len(selected))
		for _, review := range selected {
			ids = append(ids, review.ID)
		}
		want := []string{"human-approval", "source-rejection", "landing-rejection", "human-refusal", "implementation", "validator", "source-approval", "landing-approval"}
		if !slices.Equal(ids, want) {
			t.Fatalf("review selection lost gate authority: got %v want %v", ids, want)
		}
	}
}

func historyRepairSource(t *testing.T, f changeFixture, base tracker.ChangeVersion, count int) (*coreNeutralFixture, changeFixture, []tracker.ChangeVersion) {
	t.Helper()
	change := f.change
	change.ID, change.Revision, change.Landed = newNativeID("change"), 1, nil
	versions := make([]tracker.ChangeVersion, count)
	for index := range versions {
		versions[index] = base
		versions[index].ID, versions[index].ChangeID, versions[index].Number = fmt.Sprintf("%s_version_%04d", change.ID, index), change.ID, int64(index+1)
	}
	change.CurrentVersion = versions[len(versions)-1].ID
	fixture := newCoreNeutralFixture(t, f.project.OrganizationID, "change_requests", "change_issue_links", "change_versions", "change_evidence", "change_sources", "change_landing_receipts", "quality_landings", "change_viewed_files", "policy_revisions", "change_command_outputs", "change_finding_attributions", "change_content_objects", "attachments", "native_pr_publications", "release_deliveries")
	fixture.add("policy_revisions", map[string]any{"scope": string(f.project.OrganizationID) + "/" + string(f.project.ID), "policy_id": base.PolicyID, "metadata_json": string(historyTestJSON(t, base.Policy))})
	fixture.add("change_requests", map[string]any{"id": change.ID, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": f.issue.WorkItemID, "record_json": string(historyTestJSON(t, change))})
	for _, item := range change.LinkedIssues {
		fixture.add("change_issue_links", map[string]any{"change_id": change.ID, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": item})
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
		fixture.add("change_versions", map[string]any{"id": version.ID, "change_id": change.ID, "number": version.Number, "record_json": string(historyTestJSON(t, version)), "policy_revision_id": version.PolicyID, "repository": version.Repository})
		if version.Source != nil {
			fixture.add("change_sources", map[string]any{"version_id": version.ID, "bundle": bundle, "object_key": "", "object_sha256": "", "object_bytes": 0, "content_bytes": 0})
		}
	}
	f.change, f.path = change, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/changes/"+change.ID
	return fixture, f, versions
}

func historyRepairImportSource(t *testing.T, target changeFixture, fixture *coreNeutralFixture) CoreChangeImportSource {
	t.Helper()
	objects := target.service.runDetailStore()
	return CoreChangeImportSource{BundlePath: fixture.write(), OrganizationID: target.project.OrganizationID, SourceObjects: objects, Objects: objects}
}

func historyRepairImport(t *testing.T, target changeFixture, source CoreChangeImportSource, count int64) {
	t.Helper()
	got, err := ImportCoreChanges(t.Context(), target.service.config.TenantRepository.(*postgres.Repository), source)
	if err != nil || got != count {
		t.Fatalf("historical repair import count=%d want=%d error=%v", got, count, err)
	}
}

func historyRepairAttempt(t *testing.T, f changeFixture, data tracker.NativeRunData) leasestore.Attempt {
	t.Helper()
	leaseID := newNativeID("lease")
	lease := coordinatorActionLease(t, f.service, f.project.OrganizationID, f.issue.WorkItemID, leaseID)
	data.AttemptID, data.RunID, data.LeaseID, data.FencingToken, data.Sequence, data.Outcome = newNativeID("attempt"), newNativeID("run"), tracker.LeaseID(leaseID), tracker.FencingToken(lease.FencingToken), 1, "succeeded"
	content := eventstore.NewContent(f.service.runDetailStore())
	ref, err := content.PreparePayload(t.Context(), f.project.OrganizationID, historyTestJSON(t, data))
	if err != nil {
		t.Fatal(err)
	}
	event, err := content.PrepareEvent(t.Context(), f.project.OrganizationID, historyTestJSON(t, tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateType: "issue", AggregateID: f.issue.WorkItemID, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "example-runner"}, RecordedAt: f.service.config.now(), Data: tracker.CollaborationData{Run: &data}}))
	if err != nil {
		t.Fatal(err)
	}
	var local int64
	if data.Runtime != nil {
		local = data.Runtime.LocalAttemptID
	}
	var attempt leasestore.Attempt
	coordinatorFixtureWrite(t, f.service, f.project.OrganizationID, func(w tenantstore.Writer) error {
		var err error
		attempt, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: data.AttemptID, Fence: leasestore.Fence{LeaseID: leaseID, FencingToken: lease.FencingToken}, RunID: data.RunID, Sequence: 1, Status: "succeeded", LocalAttemptID: local, StartedAt: f.service.config.now(), PayloadObjectKey: ref.Key, PayloadReference: ref})
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

func historyRepairComment(t *testing.T, f changeFixture, id string, sequence int64) {
	t.Helper()
	at := f.service.config.now()
	comment := tracker.NativeComment{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: f.issue.WorkItemID, ID: id, Revision: 1, Sequence: sequence, Actor: tracker.Actor{Kind: "runner", PrincipalID: "example-runner"}, Body: "Historical comment", CreatedAt: at, UpdatedAt: at}
	content := eventstore.NewContent(f.service.runDetailStore())
	version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: f.issue.WorkItemID, RecordID: id, Revision: 1}, historyTestJSON(t, comment))
	if err != nil {
		t.Fatal(err)
	}
	fields, err := f.service.prepareWorkItemFields(t.Context(), f.project.OrganizationID, workItemRecordPlan{comment: comment})
	if err != nil {
		t.Fatal(err)
	}
	coordinatorFixtureWrite(t, f.service, f.project.OrganizationID, func(w tenantstore.Writer) error {
		if _, err := w.CreateComment(t.Context(), corestore.Comment{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: f.issue.WorkItemID, ID: id, Revision: 1, Sequence: sequence, BodyObjectKey: fields.Body, ActorKind: comment.Actor.Kind, ActorPrincipalID: comment.Actor.PrincipalID, CreatedAt: at, UpdatedAt: at}); err != nil {
			return err
		}
		_, err := w.CreateCollaborationVersion(t.Context(), version)
		return err
	})
}

type historyRepairPauseDetails struct {
	attachment.DetailStore
	marker  []byte
	failure error
	fired   bool
}

func (s *historyRepairPauseDetails) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	if !s.fired && bytes.Contains(raw, s.marker) {
		s.fired = true
		return attachment.ArchiveReference{}, s.failure
	}
	return s.DetailStore.StoreDetail(ctx, organization, raw)
}

func historyRepairValidationSource(t *testing.T, f changeFixture, base tracker.ChangeVersion, head string) (*coreNeutralFixture, changeFixture, []tracker.ChangeVersion, []leasestore.Attempt) {
	t.Helper()
	fixture, target, versions := historyRepairSource(t, f, base, changeLegacyReadLimit+2)
	attempts := make([]leasestore.Attempt, 0, changeLegacyReadLimit+1)
	for index := range changeLegacyReadLimit + 1 {
		version := versions[index]
		merge := strings.Repeat("b", 40)
		if index == 0 {
			merge = head
		}
		receipt := tracker.NativeLandingReceipt{ChangeID: target.change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: merge, BaseRef: "develop", Landed: true, ObservedAt: target.service.config.now()}
		attempt := historyRepairAttempt(t, target, tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: int64(index + 1), Landing: &receipt}})
		attempts = append(attempts, attempt)
		fixture.add("change_landing_receipts", map[string]any{"attempt_id": attempt.ID, "version_id": version.ID, "record_json": string(historyTestJSON(t, receipt)), "merge_sha": nil})
	}
	return fixture, target, versions, attempts
}
