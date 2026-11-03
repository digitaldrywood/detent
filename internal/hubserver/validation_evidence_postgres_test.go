package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresValidationAuditReadsIssueCommentsAndAttemptLandingWithoutLegacyDatabase(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	details := &memoryRunDetails{}
	service := &Service{database: &database{postgres: repository}, config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: details, Logger: discardLogger(), now: func() time.Time { return now }}}
	content := eventstore.NewContent(details)
	descriptor := hubTestPolicy()
	descriptor = descriptor.WithID()
	environment := gate.CheckEnvironment{OS: "linux", Architecture: "amd64", GoVersion: "go1.26.10"}
	localHead, scheduledHead, tree := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	localCheck := gate.CheckObservation{Scope: "lint", Command: "make lint", HeadSHA: localHead, TreeSHA: tree, Environment: environment, StartedAt: now.Add(-4 * time.Minute), FinishedAt: now.Add(-3 * time.Minute)}
	local := gate.CommandResult{Command: "make check-land", HeadSHA: localHead, TreeSHA: tree, Output: "private output must not be repeated", Evidence: &gate.CommandEvidence{Environment: environment, Checks: []gate.CheckObservation{localCheck}}}
	stamp := func(key string) string {
		sum := sha256.Sum256([]byte(key))
		check := localCheck
		check.HeadSHA = scheduledHead
		check.ExitCode = 1
		check.StartedAt = now.Add(-time.Minute)
		check.FinishedAt = now.Add(-30 * time.Second)
		scheduled := gate.ScheduledEvidence{Schema: 1, Repository: "example-org/example-project", OccurrenceKey: hex.EncodeToString(sum[:]), RunID: "123", RunAttempt: "1", JobID: "456", JobName: "Lint", RunURL: "https://github.com/example-org/example-project/actions/runs/123/attempts/1", JobURL: "https://github.com/example-org/example-project/actions/jobs/456", HeadSHA: scheduledHead, Conclusion: "failure", Checks: []gate.CheckObservation{check}}
		return issueorigin.Stamp("Scheduled check failed"+scheduled.Stamp(), issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/example-org/example-project/actions/runs/123", Fingerprint: hex.EncodeToString(sum[:])})
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		service.localOrganization = string(org)
		credential := apiCredential{ID: "reader", Hash: apikey.HashToken(string(org) + "_reader"), Scope: apiScopeOperator}
		policyRow, err := content.PrepareProjectPolicy(t.Context(), org, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		policyRow.ProjectID, policyRow.ApprovedBy, policyRow.ApprovedAt = "example", "reader", now
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "example", Name: "Example project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: credential.ID, Hash: credential.Hash, Name: "Example reader", Scope: "operator", Kind: "service", KeyScope: "read", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential.ID, ProjectID: "example"}); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: "machine", Hostname: "example", Capacity: 2, Version: "example"}); err != nil {
				return err
			}
			return w.ApproveProjectPolicy(t.Context(), policyRow, "")
		}); err != nil {
			t.Fatal(err)
		}
		for number := 1; number <= 2; number++ {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_audit_%d", number))
			actor := "reader"
			if number == 2 {
				actor = "other"
			}
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: id, Number: number, Revision: 1}, Title: "Example report", Body: stamp(string(org) + string(id)), State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: actor}, CreatedAt: now, UpdatedAt: now}
			raw, err := json.Marshal(issue)
			if err != nil {
				t.Fatal(err)
			}
			issueVersion, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
			if err != nil {
				t.Fatal(err)
			}
			issueFields, err := service.prepareWorkItemFields(t.Context(), org, workItemRecordPlan{issue: issue})
			if err != nil {
				t.Fatal(err)
			}
			var lease leasestore.Lease
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				row, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "example", NativeID: id, Number: int64(number), Title: issue.Title, BodyObjectKey: &issueFields.Body, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State, NativeCreatedAt: now, NativeUpdatedAt: now})
				if err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(t.Context(), issueVersion); err != nil {
					return err
				}
				lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: 1, MachineID: "machine", SessionID: string(id), LeaseID: newNativeID("lease"), TTL: time.Hour})
				if err != nil {
					return err
				}
				return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: org, ProjectID: "example", LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: descriptor.ID, BoundAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			event := nativeStartedEvent(tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), PolicyID: descriptor.ID, FencingToken: tracker.FencingToken(lease.FencingToken)})
			event.Data.SessionID = lease.SessionID
			event.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: int64(number * 100), Phase: "implement", HeartbeatAt: now}
			collaboration := tracker.CollaborationEvent{ID: newNativeID("event"), OrganizationID: org, ProjectID: "example", AggregateID: id, AggregateType: "issue", Type: "run.started", SchemaVersion: 1, Actor: issue.Actor, RecordedAt: now, Data: tracker.CollaborationData{Run: &event.Data}}
			eventRow, err := content.PrepareEvent(t.Context(), org, mustMarshalAuditEvent(t, collaboration))
			if err != nil {
				t.Fatal(err)
			}
			changeID, versionID := newNativeID("change"), newNativeID("version")
			change := tracker.ChangeRequest{OrganizationID: org, ProjectID: "example", WorkItemID: id, ID: changeID, Revision: 1, Title: "Example Change", LinkedIssues: []tracker.NativeWorkItemID{id}, CreatedAt: now, UpdatedAt: now}
			raw, err = json.Marshal(change)
			if err != nil {
				t.Fatal(err)
			}
			changeRow, err := content.PrepareChange(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			version := tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{Repository: "example-org/example-project", HeadSHA: localHead, PolicyID: descriptor.ID, AttemptID: event.Data.AttemptID, RunID: event.Data.RunID, Validation: &local}, ID: versionID, ChangeID: changeID, Number: 1, Policy: descriptor, Actor: issue.Actor, CreatedAt: now.Add(-2 * time.Minute)}
			raw, err = json.Marshal(version)
			if err != nil {
				t.Fatal(err)
			}
			versionRow, err := content.PrepareVersion(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			landing := tracker.NativeLandingReceipt{ChangeID: changeID, VersionID: versionID, HeadSHA: localHead, MergeSHA: scheduledHead, BaseRef: "develop", Landed: true, ObservedAt: now.Add(-2 * time.Minute), Gate: &local}
			receiptRow, err := content.PrepareLandingReceipt(t.Context(), eventstore.LandingReceipt{OrganizationID: org, AttemptID: event.Data.AttemptID, VersionID: versionID, Repository: "example-org/example-project", BaseRef: "develop"}, landing)
			if err != nil {
				t.Fatal(err)
			}
			event.Data.Runtime.Landing = &landing
			collaboration.Data.Run = &event.Data
			eventRow, err = content.PrepareEvent(t.Context(), org, mustMarshalAuditEvent(t, collaboration))
			if err != nil {
				t.Fatal(err)
			}
			change.CurrentVersion = versionID
			raw, err = json.Marshal(change)
			if err != nil {
				t.Fatal(err)
			}
			updatedChange, err := content.PrepareChange(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			comment := tracker.NativeComment{OrganizationID: org, ProjectID: "example", WorkItemID: id, ID: newNativeID("comment"), Revision: 1, Sequence: 1, Actor: issue.Actor, Body: stamp(string(org) + string(id) + "comment"), CreatedAt: now, UpdatedAt: now}
			raw, err = json.Marshal(comment)
			if err != nil {
				t.Fatal(err)
			}
			commentVersion, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: id, RecordID: comment.ID, Revision: 1}, raw)
			if err != nil {
				t.Fatal(err)
			}
			commentFields, err := service.prepareWorkItemFields(t.Context(), org, workItemRecordPlan{comment: comment})
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: event.Data.AttemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: event.Data.RunID, Sequence: 1, Status: "running", StartedAt: now, PayloadObjectKey: eventRow.PayloadObjectKey}); err != nil {
					return err
				}
				if _, err := w.AppendEvent(t.Context(), eventRow); err != nil {
					return err
				}
				if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
					return err
				}
				if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "example", WorkItemID: id, ChangeID: changeID, CreatedAt: now}); err != nil {
					return err
				}
				if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
					return err
				}
				if _, err := w.UpdateChange(t.Context(), updatedChange); err != nil {
					return err
				}
				if _, err := w.CreateLandingReceipt(t.Context(), receiptRow); err != nil {
					return err
				}
				if _, err := w.CreateComment(t.Context(), corestore.Comment{OrganizationID: org, ProjectID: "example", WorkItemID: id, ID: comment.ID, Revision: 1, Sequence: 1, BodyObjectKey: commentFields.Body, ActorKind: comment.Actor.Kind, ActorPrincipalID: comment.Actor.PrincipalID, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				_, err := w.CreateCollaborationVersion(t.Context(), commentVersion)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: org, project: "example", credential: credential}
			identity := operatorIdentity(credential, string(org))
			ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
				return operatortool.Authority{Identity: identity, Check: func(ctx context.Context, _ operatortool.Requirement) error {
					return repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error { return service.authorizeAccessScope(ctx, r, scope, false) })
				}}, nil
			}})
			for _, tool := range []string{operatortool.WorkItem, operatortool.WorkAttemptReceipt} {
				t.Run(string(org)+"/"+string(id)+"/"+tool, func(t *testing.T) {
					defer func() {
						if value := recover(); value != nil {
							t.Fatalf("validation audit panicked with nil legacy database: %v", value)
						}
					}()
					result, err := (operatorWorkReads{service: service, scope: scope}).ReadWork(ctx, tool, operatortool.WorkReadRequest{ProjectID: "example", Reference: string(id), NativeAttemptID: event.Data.AttemptID, Limit: 10})
					if err != nil {
						t.Fatal(err)
					}
					var value struct {
						Freshness string `json:"freshness"`
						Data      struct {
							ValidationAudit *tracker.ValidationAudit `json:"validation_audit"`
							Attempt         *tracker.NativeAttempt   `json:"attempt"`
							Body            string                   `json:"body"`
						} `json:"data"`
					}
					if err := json.Unmarshal(result.Content, &value); err != nil {
						t.Fatal(err)
					}
					audit := value.Data.ValidationAudit
					if audit == nil || !audit.Partial || !slices.Contains(audit.Unavailable, "project_wide_scheduled_sources") || len(audit.Occurrences) != 2 {
						t.Fatalf("missing real issue/comment audit or honest partial coverage")
					}
					for _, occurrence := range audit.Occurrences {
						if occurrence.Source.WorkItemID != id || occurrence.Source.Actor.PrincipalID != actor || len(occurrence.Comparisons) != 1 {
							t.Fatal("audit lost current scoped source identity")
						}
						comparison := occurrence.Comparisons[0]
						if comparison.VersionID != versionID || comparison.ChangeID != changeID || comparison.IntegratedSource != "same_commit" || comparison.Outcome != "local_pass_scheduled_failure" || comparison.Local == nil || comparison.Local.Output != "" {
							t.Fatalf("audit lost verified local validation comparison: outcome=%s", comparison.Outcome)
						}
						if tool == operatortool.WorkAttemptReceipt && (comparison.NativeAttemptID != event.Data.AttemptID || comparison.NativeRunID != event.Data.RunID || comparison.LocalAttemptID != int64(number*100)) {
							t.Fatal("audit lost selected attempt binding")
						}
					}
					if tool == operatortool.WorkItem && value.Data.Body != issue.Body {
						t.Fatal("issue detail body was lost")
					}
					if tool == operatortool.WorkAttemptReceipt && (value.Data.Attempt == nil || value.Data.Attempt.AttemptID != event.Data.AttemptID) {
						t.Fatal("selected attempt detail was lost")
					}
				})
			}
		}
	}
}

func mustMarshalAuditEvent(t *testing.T, value tracker.CollaborationEvent) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
