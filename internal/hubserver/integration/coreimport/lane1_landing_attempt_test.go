package coreimport_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeColumnsSelectAttemptEvidence(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	matchingMerge := strings.Repeat("1", 40)
	otherMerge := strings.Repeat("3", 40)
	for _, test := range []struct {
		name          string
		typedReceipts int
		historical    bool
		want          int
	}{
		{name: "present", typedReceipts: 2, want: 1},
		{name: "absent", typedReceipts: 1, want: 0},
		{name: "unknown", historical: true, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := hubfixture.NewChangeInOrganization(t, h)
			evidence := seedLandingAttemptPair(t, f, matchingMerge, test.typedReceipts, test.historical)
			historyRepairAttempt(t, f, &tracker.NativeRuntimeObservation{LocalAttemptID: 102, Phase: "completed", HeartbeatAt: h.Now()})
			want := evidence[test.want]
			var selected eventstore.LandingReceipt
			if err := h.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				selected, err = r.LatestLandingReceipt(t.Context(), f.Project.ID, f.Issue.WorkItemID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if selected.AttemptID != want.attempt.ID || selected.VersionID != want.version.ID {
				t.Fatalf("landing attempt=%s version=%s want attempt=%s version=%s", selected.AttemptID, selected.VersionID, want.attempt.ID, want.version.ID)
			}
		})
	}
	for _, test := range []struct {
		name          string
		oldMerge      string
		typedReceipts int
		historical    bool
		first         int
	}{
		{name: "typed priority", oldMerge: matchingMerge, typedReceipts: 2, first: 0},
		{name: "typed mismatch", oldMerge: otherMerge, typedReceipts: 2, first: 1},
		{name: "unknown matching receipt ranks first", oldMerge: matchingMerge, historical: true, first: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := hubfixture.NewNamedChange(t, h, test.name)
			evidence := seedLandingAttemptPair(t, f, test.oldMerge, test.typedReceipts, test.historical)
			repository := strings.TrimPrefix(evidence[1].version.Repository, "https://github.com/")
			rows := readLandingAttemptComparisons(t, f, repository, matchingMerge)
			want := []landingAttemptEvidence{evidence[test.first], evidence[1-test.first]}
			if !slices.EqualFunc(rows, want, func(row tracker.ValidationComparison, candidate landingAttemptEvidence) bool {
				return row.VersionID == candidate.version.ID && row.NativeAttemptID == candidate.attempt.ID && row.LocalAttemptID == candidate.localID && row.ChangeID == f.Change.ID
			}) {
				t.Fatalf("ranked comparisons=%+v want version/attempt/local=%s/%s/%d then %s/%s/%d", rows, want[0].version.ID, want[0].attempt.ID, want[0].localID, want[1].version.ID, want[1].attempt.ID, want[1].localID)
			}
		})
	}
	t.Run("typed receipt mismatch fails integrity", func(t *testing.T) {
		f := hubfixture.NewChangeInOrganization(t, h)
		version := f.Publish(t, "version", "")
		receipt := attemptLandingReceipt(f, version, matchingMerge)
		attempt := historyRepairAttempt(t, f, &tracker.NativeRuntimeObservation{LocalAttemptID: 100, Phase: "completed", HeartbeatAt: h.Now(), Landing: &receipt})
		row := prepareAttemptLandingReceipt(t, f, version, attempt, receipt)
		row.MergeSHA = otherMerge
		writeAttemptLandingReceipt(t, f, row, false)
		if err := h.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			row, err = r.LandingReceipt(t.Context(), attempt.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if row.MergeSHA != otherMerge {
			t.Fatalf("stored typed merge=%q want=%q", row.MergeSHA, otherMerge)
		}
		if _, err := eventstore.NewContent(h.Config.RunDetails).LandingReceipt(t.Context(), row); !errors.Is(err, artifact.ErrIntegrity) {
			t.Fatalf("contradictory canonical receipt accepted: %v", err)
		}
	})
}

type landingAttemptEvidence struct {
	version tracker.ChangeVersion
	attempt leasestore.Attempt
	localID int64
}

func seedLandingAttemptPair(t *testing.T, f hubfixture.Change, oldMerge string, typedReceipts int, historical bool) [2]landingAttemptEvidence {
	t.Helper()
	old := f.Publish(t, "old", "")
	current := f.Publish(t, "current", old.ID)
	versions := [2]tracker.ChangeVersion{old, current}
	merges := [2]string{oldMerge, strings.Repeat("2", 40)}
	var evidence [2]landingAttemptEvidence
	for i, version := range versions {
		receipt := attemptLandingReceipt(f, version, merges[i])
		localID := int64(100 + i)
		attempt := historyRepairAttempt(t, f, &tracker.NativeRuntimeObservation{LocalAttemptID: localID, Phase: "completed", HeartbeatAt: f.Hub.Now(), Landing: &receipt})
		evidence[i] = landingAttemptEvidence{version: version, attempt: attempt, localID: localID}
		if i < typedReceipts || historical {
			writeAttemptLandingReceipt(t, f, prepareAttemptLandingReceipt(t, f, version, attempt, receipt), historical)
		}
	}
	return evidence
}

func attemptLandingReceipt(f hubfixture.Change, version tracker.ChangeVersion, merge string) tracker.NativeLandingReceipt {
	return tracker.NativeLandingReceipt{ChangeID: f.Change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: merge, BaseRef: "develop", Landed: true, ObservedAt: f.Hub.Now()}
}

func prepareAttemptLandingReceipt(t *testing.T, f hubfixture.Change, version tracker.ChangeVersion, attempt leasestore.Attempt, receipt tracker.NativeLandingReceipt) eventstore.LandingReceipt {
	t.Helper()
	row, err := eventstore.NewContent(f.Hub.Config.RunDetails).PrepareLandingReceipt(t.Context(), eventstore.LandingReceipt{OrganizationID: f.Project.OrganizationID, AttemptID: attempt.ID, VersionID: version.ID, Repository: version.Repository, BaseRef: receipt.BaseRef}, receipt)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func writeAttemptLandingReceipt(t *testing.T, f hubfixture.Change, row eventstore.LandingReceipt, historical bool) {
	t.Helper()
	if historical {
		if err := f.Hub.Repository.WriteRecords(t.Context(), f.Project.OrganizationID, corestore.Background, func(w eventstore.Writer) error {
			_, err := w.CreateLandingReceipt(t.Context(), row)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	writeLegacyChange(t, f, func(w tenantstore.Writer) error {
		_, err := w.CreateLandingReceipt(t.Context(), row)
		return err
	})
}

func readLandingAttemptComparisons(t *testing.T, f hubfixture.Change, repository, head string) []tracker.ValidationComparison {
	t.Helper()
	fingerprint := artifact.Digest([]byte(f.Change.ID))
	runURL := "https://github.com/" + repository + "/actions/runs/123"
	scheduled := gate.ScheduledEvidence{Schema: 1, Repository: repository, OccurrenceKey: fingerprint, RunID: "123", RunAttempt: "1", JobID: "456", JobName: "Check", RunURL: runURL + "/attempts/1", JobURL: "https://github.com/" + repository + "/actions/jobs/456", HeadSHA: head, Conclusion: "failure"}
	body := issueorigin.Stamp("Scheduled check failed"+scheduled.Stamp(), issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: runURL, Fingerprint: fingerprint})
	path := f.Base + "/work-items/" + string(f.Issue.WorkItemID)
	issueBody := f.Issue.Body + "\n\nSee the detent-checks evidence in the follow-up comment."
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPatch, path, f.Token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "validation-issue"}, ExpectedRevision: f.Issue.Revision, Body: &issueBody}), http.StatusOK)
	response := f.Hub.Request(t, http.MethodPost, path+"/comments", f.Token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "scheduled-source"}, Body: body})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var comment tracker.NativeComment
	hubfixture.Decode(t, response, &comment)
	tool := operatortool.WorkItem
	call := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": tool, "_meta": json.RawMessage(hubfixture.MCPProtocolMeta),
		"arguments": map[string]any{"project_id": f.Project.ID, "reference": f.Issue.WorkItemID},
	}}
	response = f.Hub.WorkCall(t, hubfixture.OrganizationBase(f.Project.OrganizationID)+"/mcp", f.Token, tool, call)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var envelope struct {
		Result struct {
			IsError    bool `json:"isError"`
			Structured struct {
				Data struct {
					ValidationAudit *tracker.ValidationAudit `json:"validation_audit"`
				} `json:"data"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	hubfixture.Decode(t, response, &envelope)
	audit := envelope.Result.Structured.Data.ValidationAudit
	if envelope.Result.IsError || audit == nil || !audit.Partial || !slices.Equal(audit.Unavailable, []string{"unlanded_historical_versions", "historical_repository_identity"}) || len(audit.Occurrences) != 1 {
		t.Fatalf("validation audit=%+v MCP error=%v", audit, envelope.Result.IsError)
	}
	occurrence := audit.Occurrences[0]
	if comment.ID == "" || occurrence.Source.RecordID != comment.ID || occurrence.Scheduled.OccurrenceKey != fingerprint {
		t.Fatalf("validation source=%+v want comment=%s occurrence=%s", occurrence.Source, comment.ID, fingerprint)
	}
	return occurrence.Comparisons
}
