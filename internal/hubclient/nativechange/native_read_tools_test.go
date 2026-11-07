package nativechange

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeExecutionRetainedRecoveryEvidence(t *testing.T) {
	h := newNativeChangeHub(t, true)
	cfg := config.Default()
	cfg.Tracker.Kind = config.TrackerHubNative
	descriptor, err := config.ResolvePolicy(config.Workflow{Config: cfg, Prompt: strings.Repeat("p", 320<<10)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: h.descriptor.ID, Policy: descriptor}); err != nil {
		t.Fatal(err)
	}
	h.descriptor = descriptor
	issue := h.createInProgress(t, "Retained evidence worker")
	body := issueContractTestSections + strings.Repeat("<", 153<<10) + strings.Repeat("b", (256<<10)-len(issueContractTestSections)-(153<<10))
	currentIssue, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.UpdateIssue(t.Context(), currentIssue.WorkItemID, tracker.UpdateIssue{Mutation: nativeMutationKey(), ExpectedRevision: currentIssue.Revision, Body: &body}); err != nil {
		t.Fatal(err)
	}
	h.claim(t, issue.ID)
	execution := h.scheduler.RunExecution(issue.ID)
	if execution == nil {
		t.Fatal("claimed worker execution is missing")
	}
	if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "rework", Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	tools, handler := execution.(runner.ToolExecution).AgentTools()
	var schema json.RawMessage
	for _, tool := range tools {
		if tool.Name == operatortool.WorkItem {
			schema = tool.InputSchema
		}
	}

	items := []tracker.NativeWorkItemID{tracker.NativeWorkItemID(issue.ID)}
	other := createIndependentNativeIssue(t, h.admin, tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Second retained item", Body: body, State: "Todo"})
	items = append(items, other.WorkItemID)
	type evidence struct {
		change   tracker.ChangeRequest
		versions []tracker.ChangeVersion
		reviews  []tracker.ChangeReview
		comment  tracker.NativeComment
		landing  *tracker.SecurityAuditReview
	}
	wants := make(map[tracker.NativeWorkItemID]evidence)
	for i, item := range items {
		change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: fmt.Sprintf("Retained change %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		old := h.publish(t, item, change.ID, nativeChangeFixtureHead("b"))
		current := h.publish(t, item, change.ID, nativeChangeFixtureHead("c"), old.ID)
		comment, err := h.admin.CreateComment(t.Context(), item, tracker.CreateComment{Mutation: nativeMutationKey(), Body: strings.Repeat("retained discussion <\n", 2800)})
		if err != nil {
			t.Fatal(err)
		}
		reviews := []tracker.ChangeReview{
			{ID: fmt.Sprintf("review_human_%d", i), VersionID: current.ID, Decision: "changes_requested", Body: strings.Repeat("human feedback <\n", 65536), Actor: tracker.Actor{Kind: "human", PrincipalID: "reviewer"}, CreatedAt: current.CreatedAt},
			{ID: fmt.Sprintf("review_commands_%d", i), VersionID: old.ID, Decision: "approved", Actor: tracker.Actor{Kind: "runner", PrincipalID: "worker"}, CreatedAt: old.CreatedAt, ImplementationReview: &gate.ValidatorResult{Attribution: "unknown", VersionID: old.ID, Repository: old.Repository, BaseSHA: old.BaseSHA, HeadSHA: old.HeadSHA, Submitted: true, Verdict: "pass", Commands: []gate.CommandResult{{Attribution: "unknown", Command: "make check-land", Output: strings.Repeat("command output <\n", 40000)}}}},
		}
		now := time.Now().UTC()
		run := securityaudit.Run{Attribution: "unknown", ID: int64(42 + i), InvocationID: fmt.Sprintf("audit-%d", i), ProjectID: string(h.project), IssueID: string(item), Repository: current.Repository, BaseSHA: current.BaseSHA, HeadSHA: current.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now, CompletedAt: now, ExitStatus: securityaudit.ExitStatusSuccess, OutputBytes: 1, OutputDigest: policy.Digest([]byte("audit")), Verdict: "pass", Summary: strings.Repeat("s", (1<<20)-(16<<10))}
		audit := &tracker.SecurityAuditReview{VersionID: current.ID, SessionID: run.ID, Verdict: "pass", DiffDigest: policy.Digest([]byte("diff")), Run: run}
		decision := "approved"
		if i == 1 {
			audit.Run.Summary = "Blocking audit"
			audit.Run.Verdict, audit.Verdict, decision = "fail", "rework", "changes_requested"
			audit.Run.Findings = []securityaudit.Finding{{Attribution: "unknown", ID: "blocking", Severity: "p2", Body: strings.Repeat("finding <\n", 90_000)}, {Attribution: "unknown", ID: "resolved", Severity: "p1", Status: "resolved", Body: "Already addressed"}}
		}
		reviews = append(reviews, tracker.ChangeReview{ID: fmt.Sprintf("review_audit_%d", i), VersionID: current.ID, Decision: decision, SecurityAudit: audit, Actor: tracker.Actor{Kind: "runner", PrincipalID: "audit-worker"}, CreatedAt: current.CreatedAt})
		var landing *tracker.SecurityAuditReview
		if i == 0 {
			retained := *audit
			retained.Landing = true
			retained.Run.Summary = "All findings resolved"
			for finding := range 6000 {
				retained.Run.Findings = append(retained.Run.Findings, securityaudit.Finding{ID: fmt.Sprintf("finding-%06d-%s", finding, strings.Repeat("i", 64)), Status: "resolved", Severity: "p1", Body: "Resolved"})
			}
			landing = &retained
			observation := tracker.NativeRuntimeObservation{Phase: "merging", Landing: &tracker.NativeLandingReceipt{SecurityAudit: landing, ChangeID: change.ID, VersionID: current.ID, HeadSHA: current.HeadSHA, GateFailed: true, ObservedAt: now}}
			if err := execution.(runner.RuntimeExecution).ObserveRuntime(t.Context(), observation); err != nil {
				t.Fatal(err)
			}
			if err := execution.(*nativeExecution).append(t.Context(), "run.observed", "", nil, true); err != nil {
				t.Fatal(err)
			}
		}
		wants[item] = evidence{change: change, versions: []tracker.ChangeVersion{old, current}, reviews: reviews, comment: comment, landing: landing}
	}
	h.reopen(func(w tenantstore.Writer) error {
		for _, want := range wants {
			for _, review := range want.reviews {
				raw, err := json.Marshal(review)
				if err != nil {
					return err
				}
				row, err := eventstore.NewContent(h.config.RunDetails).PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: h.organization, ChangeID: want.change.ID, Kind: "review"}, raw)
				if err != nil {
					return err
				}
				_, err = w.AppendEvidence(t.Context(), row)
				if err != nil {
					return err
				}
			}
		}
		return nil
	})
	for _, item := range items {
		t.Run(string(item), func(t *testing.T) {
			want := wants[item]
			recovery, err := h.native.Recovery(t.Context(), item)
			if err != nil {
				t.Fatalf("large audit prevented worker recovery: %v", err)
			}
			if recovery.Issue.Body != body || recovery.Read == nil || recovery.Read.IssueBody == nil {
				t.Fatal("combined evidence lost required instructions before worker dispatch")
			}
			if recovery.ChangeDetail == nil || recovery.Read == nil || !slices.Contains(recovery.Read.Omitted, "change_detail.reviews") {
				t.Fatal("projected audit did not expose retained evidence")
			}
			if want.landing != nil {
				copies := []*tracker.NativeLandingReceipt{recovery.ChangeDetail.Versions[0].Landing}
				for _, attempt := range recovery.Attempts {
					if attempt.Runtime != nil && attempt.Runtime.Landing != nil {
						copies = append(copies, attempt.Runtime.Landing)
					}
				}
				if len(copies) != 2 {
					t.Fatal("worker lost a landing copy")
				}
				for _, receipt := range copies {
					if receipt == nil || receipt.SecurityAudit == nil || !receipt.SecurityAudit.RunOmitted || receipt.SecurityAudit.SessionID != want.landing.SessionID || receipt.SecurityAudit.Verdict != want.landing.Verdict || !receipt.GateFailed {
						t.Fatal("worker lost bounded landing identity or gate result")
					}
				}
			}
			original := want.reviews[len(want.reviews)-1]
			var projected tracker.ChangeReview
			for _, review := range recovery.ChangeDetail.Reviews {
				if review.ID == original.ID {
					projected = review
				}
			}
			if projected.SecurityAudit == nil || !projected.SecurityAudit.TextTruncated || projected.Decision != original.Decision || projected.VersionID != original.VersionID {
				t.Fatal("bounded recovery lost authoritative audit identity or decision")
			}
			got, expected := *projected.SecurityAudit, *original.SecurityAudit
			if len(got.Run.Summary) > 4096 || strings.TrimSpace(got.Run.Summary) == "" || len(got.Run.Findings) != len(expected.Run.Findings) {
				t.Fatal("audit prose remained unbounded or lost finding state")
			}
			key := securityaudit.Key{ProjectID: string(h.project), Repository: expected.Run.Repository, BaseSHA: expected.Run.BaseSHA, HeadSHA: expected.Run.HeadSHA}
			for _, severities := range [][]string{{"p1", "p2"}, {"p3"}, {"none"}} {
				actual := securityaudit.Evaluate(got.Run, nil, key, "", severities)
				before := securityaudit.Evaluate(expected.Run, nil, key, "", severities)
				if actual.Allowed != before.Allowed || actual.Reason != before.Reason || len(actual.Findings) != len(before.Findings) {
					t.Fatal("projection changed audit validity or configured blocking semantics")
				}
			}
			got.TextTruncated = false
			got.Run.Summary = expected.Run.Summary
			got.Run.Findings = slices.Clone(got.Run.Findings)
			for i := range got.Run.Findings {
				if len(got.Run.Findings[i].Body) > 4096 || len(got.Run.Findings[i].Path) > 4096 {
					t.Fatal("unbounded finding prose")
				}
				got.Run.Findings[i].Body, got.Run.Findings[i].Path = expected.Run.Findings[i].Body, expected.Run.Findings[i].Path
			}
			if !reflect.DeepEqual(got, expected) {
				t.Fatal("projection changed audit identity, verdict, timestamps, digest or finding ID/status/severity")
			}
			seen := map[string]bool{}
			cursor := ""
			var record []byte
			var identity tracker.NativeRecoveryPage
			for pages := 0; ; pages++ {
				if pages > 200 {
					t.Fatal("retained traversal did not terminate")
				}
				args := map[string]any{"project_id": h.project, "reference": item, "view": "recovery"}
				if cursor != "" {
					args["cursor"] = cursor
				}
				raw, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				result, err := handler(t.Context(), runner.AgentToolCall{Name: operatortool.WorkItem, Arguments: raw})
				var response operatortool.WorkReadResult[tracker.NativeRecoveryPage]
				if err != nil || !result.Success || len(result.Content) > operatortool.MaxResultBytes || json.Unmarshal([]byte(result.Content), &response) != nil {
					t.Fatalf("worker retained read: success=%t bytes=%d err=%v", result.Success, len(result.Content), err)
				}
				if !strings.Contains(string(schema), "view") {
					t.Fatal("worker work_item schema cannot request retained recovery evidence")
				}
				page := response.Data
				if page.WorkItemID != item || page.ProjectID != h.project || page.OrganizationID != h.organization {
					t.Fatal("fragment lost scoped identity")
				}
				if page.Section == "complete" {
					if len(record) != 0 {
						t.Fatal("truncated final record")
					}
					break
				}
				if page.Offset != int64(len(record)) || len(page.Data) == 0 || len(page.Data) > 64<<10 || page.NextCursor == "" || page.NextCursor == cursor {
					t.Fatal("unbounded or non-progressing fragment")
				}
				if page.Offset == 0 {
					identity = page
				} else if page.Section != identity.Section || page.RecordID != identity.RecordID || page.VersionID != identity.VersionID || page.TotalBytes != identity.TotalBytes || page.SHA256 != identity.SHA256 {
					t.Fatal("fragment identity changed")
				}
				if cursor == "" {
					foreignItem := items[0]
					if item == foreignItem {
						foreignItem = items[1]
					}
					foreignArgs, err := json.Marshal(map[string]any{"project_id": h.project, "reference": foreignItem, "view": "recovery", "cursor": page.NextCursor})
					if err != nil {
						t.Fatal(err)
					}
					refusal, err := handler(t.Context(), runner.AgentToolCall{Name: operatortool.WorkItem, Arguments: foreignArgs})
					if err == nil || refusal.Success {
						t.Fatal("worker accepted another item's cursor")
					}
				}
				record = append(record, page.Data...)
				if int64(len(record)) == page.TotalBytes {
					digest := sha256.Sum256(record)
					if hex.EncodeToString(digest[:]) != page.SHA256 {
						t.Fatal("retained record digest changed")
					}
					switch page.Section {
					case "reviews":
						var got tracker.ChangeReview
						if err := json.Unmarshal(record, &got); err != nil {
							t.Fatal(err)
						}
						for _, review := range want.reviews {
							if review.ID == got.ID {
								if page.VersionID != review.VersionID || !reflect.DeepEqual(got, review) {
									t.Fatal("worker lost complete ordinary review or command output")
								}
								seen[review.ID] = true
							}
						}
					case "versions":
						var got tracker.ChangeVersion
						if err := json.Unmarshal(record, &got); err != nil {
							t.Fatal(err)
						}
						for _, version := range want.versions {
							if version.ID == got.ID {
								if page.VersionID != version.ID || !reflect.DeepEqual(got.Policy, version.Policy) || got.HeadSHA != version.HeadSHA || !reflect.DeepEqual(got.Source, version.Source) {
									t.Fatal("worker lost immutable source or historical frozen policy")
								}
								if want.landing != nil && version.ID == want.versions[len(want.versions)-1].ID && (got.Landing == nil || !reflect.DeepEqual(got.Landing.SecurityAudit, want.landing)) {
									t.Fatal("worker lost exact version landing audit")
								}
								seen[version.ID] = true
							}
						}
					case "attempts":
						var got tracker.NativeAttempt
						if err := json.Unmarshal(record, &got); err != nil {
							t.Fatal(err)
						}
						if want.landing != nil && got.Runtime != nil && got.Runtime.Landing != nil {
							if !reflect.DeepEqual(got.Runtime.Landing.SecurityAudit, want.landing) || !got.Runtime.Landing.GateFailed {
								t.Fatal("worker lost exact attempt landing audit")
							}
							seen[page.RecordID] = true
						}
					case "discussion":
						var got tracker.NativeComment
						if err := json.Unmarshal(record, &got); err != nil {
							t.Fatal(err)
						}
						if got.ID == want.comment.ID {
							if !reflect.DeepEqual(got, want.comment) {
								t.Fatal("worker lost retained discussion")
							}
							seen[got.ID] = true
						}
					}
					record = nil
				}
				cursor = page.NextCursor
			}
			expectedRecords := 6
			if want.landing != nil {
				expectedRecords++
			}
			if len(seen) != expectedRecords {
				t.Fatalf("worker lost retained evidence: %v", seen)
			}
		})
	}
}
