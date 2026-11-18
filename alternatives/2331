package validation_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresValidationAuditReadsIssueCommentsAndAttemptLandingWithoutLegacyDatabase(t *testing.T) {
	admin := hubtest.Database(t)
	repository := hubtest.RepositoryOn(t, admin)
	now := time.Now().UTC().Truncate(time.Microsecond)
	details := hubtest.NewDetails()
	connection := hubtest.PostgresConfig(t, admin)
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		switch name {
		case "DETENT_HUB_SECRET_KEYS":
			return `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6b}, 32)) + `"}`
		case "DETENT_HUB_SECRET_KEY_VERSION":
			return "1"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	recorded := &operatorErrors{}
	service, err := hubserver.Open(t.Context(), hubserver.Config{DatabaseBackend: hubserver.DatabasePostgres, Postgres: &connection, TenantRepository: repository, AccessRepository: repository, RunDetails: details, Logger: slog.New(recorded), Clock: func() time.Time { return now }, InitialAdminToken: []byte("example_admin_token"), SecretKeys: keys})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})

	content := eventstore.NewContent(details)
	descriptor := hubtest.PolicyDescriptor()
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
		credential := accessstore.Credential{ID: "reader", Hash: apikey.HashToken(string(org) + "_reader"), Scope: "operator"}
		policyRow, err := content.PrepareProjectPolicy(t.Context(), org, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		policyRow.ProjectID, policyRow.ApprovedBy, policyRow.ApprovedAt = "example", "reader", now
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org)}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "example", Name: "Example project", Profile: "native"}, States: hubtest.States()}); err != nil {
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
			issueBody, err := json.Marshal(issue.Body)
			if err != nil {
				t.Fatal(err)
			}
			issueFields, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, issueBody)
			if err != nil {
				t.Fatal(err)
			}
			var lease leasestore.Lease
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				row, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "example", NativeID: id, Number: int64(number), Title: issue.Title, BodyObjectKey: &issueFields.PayloadObjectKey, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State, NativeCreatedAt: now, NativeUpdatedAt: now})
				if err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(t.Context(), issueVersion); err != nil {
					return err
				}
				lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: 1, MachineID: "machine", SessionID: string(id), LeaseID: hubapi.NewID("lease"), TTL: time.Hour})
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
			runRaw, err := json.Marshal(event.Data)
			if err != nil {
				t.Fatal(err)
			}
			runPayload, err := content.PreparePayload(t.Context(), org, runRaw)
			if err != nil {
				t.Fatal(err)
			}
			collaboration := tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: org, ProjectID: "example", AggregateID: id, AggregateType: "issue", Type: "run.started", SchemaVersion: 1, Actor: issue.Actor, RecordedAt: now, Data: tracker.CollaborationData{Run: &event.Data}}
			eventRow, err := content.PrepareEvent(t.Context(), org, mustMarshalAuditEvent(t, collaboration))
			if err != nil {
				t.Fatal(err)
			}
			changeID, versionID := hubapi.NewID("change"), hubapi.NewID("version")
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
			comment := tracker.NativeComment{OrganizationID: org, ProjectID: "example", WorkItemID: id, ID: hubapi.NewID("comment"), Revision: 1, Sequence: 1, Actor: issue.Actor, Body: stamp(string(org) + string(id) + "comment"), CreatedAt: now, UpdatedAt: now}
			raw, err = json.Marshal(comment)
			if err != nil {
				t.Fatal(err)
			}
			commentVersion, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: id, RecordID: comment.ID, Revision: 1}, raw)
			if err != nil {
				t.Fatal(err)
			}
			commentBody, err := json.Marshal(comment.Body)
			if err != nil {
				t.Fatal(err)
			}
			commentFields, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, commentBody)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: event.Data.AttemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: event.Data.RunID, Sequence: 1, Status: "running", StartedAt: now, PayloadObjectKey: runPayload.Key, PayloadReference: runPayload, LocalAttemptID: event.Data.Runtime.LocalAttemptID, RuntimeHeartbeatAt: &now}); err != nil {
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
				if _, err := w.CreateComment(t.Context(), corestore.Comment{OrganizationID: org, ProjectID: "example", WorkItemID: id, ID: comment.ID, Revision: 1, Sequence: 1, BodyObjectKey: commentFields.PayloadObjectKey, ActorKind: comment.Actor.Kind, ActorPrincipalID: comment.Actor.PrincipalID, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				_, err := w.CreateCollaborationVersion(t.Context(), commentVersion)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, tool := range []string{operatortool.WorkItem, operatortool.WorkAttemptReceipt} {
				t.Run(string(org)+"/"+string(id)+"/"+tool, func(t *testing.T) {
					defer func() {
						if value := recover(); value != nil {
							t.Fatalf("validation audit panicked with nil legacy database: %v", value)
						}
					}()
					result, err := callWork(t, service, recorded, org, string(org)+"_reader", tool, operatortool.WorkReadRequest{ProjectID: "example", Reference: string(id), NativeAttemptID: event.Data.AttemptID, Limit: 10})
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
					if audit == nil || !audit.Partial || !slices.Contains(audit.Unavailable, "historical_repository_identity") || slices.Contains(audit.Unavailable, "project_wide_scheduled_sources") || slices.Contains(audit.Unavailable, "incomplete_scheduled_sources") || len(audit.Occurrences) != 2 {
						t.Fatalf("missing real issue/comment audit or honest partial coverage: %+v", audit)
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

func TestMain(m *testing.M) { hubtest.Main(m) }

func nativeStartedEvent(lease tracker.NativeLease) tracker.NativeRunEvent {
	return tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "start"}, Type: "run.started", SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test-model"},
			LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: lease.PolicyID, RunID: hubapi.NewID("run"), AttemptID: hubapi.NewID("attempt")}}
}

func callWork(t *testing.T, service *hubserver.Service, recorded *operatorErrors, org tracker.OrganizationID, token, tool string, request operatortool.WorkReadRequest) (operatortool.Result, error) {
	t.Helper()
	arguments := map[string]any{"project_id": request.ProjectID, "reference": request.Reference}
	if tool == operatortool.WorkAttemptReceipt {
		arguments["native_attempt_id"] = request.NativeAttemptID
	}
	call := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": arguments, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]string{"name": "validation-test", "version": "1"}}}}
	raw, err := json.Marshal(call)
	if err != nil {
		return operatortool.Result{}, err
	}
	httpRequest := httptest.NewRequest(http.MethodPost, "/api/v2/organizations/"+string(org)+"/mcp", bytes.NewReader(raw))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	httpRequest.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	httpRequest.Header.Set("Mcp-Method", "tools/call")
	httpRequest.Header.Set("Mcp-Name", tool)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httpRequest)
	var envelope struct {
		Result struct {
			IsError    bool            `json:"isError"`
			Structured json.RawMessage `json:"structuredContent"`
			Content    []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		return operatortool.Result{}, err
	}
	if envelope.Result.IsError || response.Code != http.StatusOK {
		if err := recorded.last(); err != nil {
			for range 2 {
				if cause := errors.Unwrap(err); cause != nil {
					err = cause
				}
			}
			return operatortool.Result{}, err
		}
		return operatortool.Result{}, errors.New(response.Body.String())
	}
	return operatortool.Result{Content: envelope.Result.Structured}, nil
}

type operatorErrors struct {
	mu  sync.Mutex
	err error
}

func (h *operatorErrors) Enabled(context.Context, slog.Level) bool { return true }
func (h *operatorErrors) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "operator tool failed" {
		record.Attrs(func(a slog.Attr) bool {
			if a.Key == "error" {
				h.mu.Lock()
				h.err, _ = a.Value.Any().(error)
				h.mu.Unlock()
			}
			return true
		})
	}
	return nil
}
func (h *operatorErrors) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *operatorErrors) WithGroup(string) slog.Handler      { return h }
func (h *operatorErrors) last() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	err := h.err
	h.err = nil
	return err
}
