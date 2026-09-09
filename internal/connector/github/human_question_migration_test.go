package github

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestHumanQuestionMigrationSelection(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                   string
		body                   string
		linked, badHash, valid bool
	}{
		{name: "explicit unresolved question", linked: true, valid: true},
		{name: "standalone human task"},
		{name: "changed source", linked: true, badHash: true},
		{name: "software dependency", linked: true, body: "Implement software"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tracker := newPrerequisiteTracker(t)
			body := tt.body
			if body == "" {
				body = prerequisiteBody(t)
			}
			tracker.issues[10] = restIssue{ID: 10, NodeID: "I_10", Number: 10, State: "open", Body: &body}
			if tt.linked {
				tracker.edges[1] = []int{10}
			}
			hash := sha256.Sum256([]byte(body))
			request := connector.HumanQuestionMigration{Dependent: "owner/repo#1", Source: "owner/repo#10", SourceBodySHA256: hex.EncodeToString(hash[:]), Question: "Use a manual handoff?"}
			if tt.badHash {
				request.SourceBodySHA256 = "changed"
			}
			_, err := tracker.connector(t).PrepareHumanQuestionMigration(t.Context(), request)
			if (err == nil) != tt.valid {
				t.Fatalf("prepare = %v", err)
			}
			if tracker.creates != 0 || tracker.edgeWrites != 0 || tracker.bodyWrites != 0 {
				t.Fatal("preparation mutated tracker")
			}
		})
	}
}

func TestRemoveQuestionDependency(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, body, want string }{
		{name: "selected only", body: "Acceptance\nDepends on: #685\n", want: "Acceptance\n"},
		{name: "mixed dependencies", body: "Depends on: #685, #20", want: "Depends on: owner/repo#20"},
		{name: "genuine dependency", body: "Depends on: #20", want: "Depends on: #20"},
		{name: "history", body: "Context: #685\n```text\nDepends on: #685\n```", want: "Context: #685\n```text\nDepends on: #685\n```"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := removeQuestionDependency(tt.body, "owner/repo", "owner/repo#685")
			if err != nil || got != tt.want {
				t.Fatalf("remove = %q, %v", got, err)
			}
			again, err := removeQuestionDependency(got, "owner/repo", "owner/repo#685")
			if err != nil || again != got {
				t.Fatalf("retry = %q, %v", again, err)
			}
		})
	}
}

func TestRemoveQuestionWorkpadBlocker(t *testing.T) {
	t.Parallel()
	for _, independent := range []bool{false, true} {
		body := "## Codex Workpad\nHistory remains.\n```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: owner/repo#685\n    owner: human\nhuman_action: null\n"
		if independent {
			body += "reason_code: spend_park\n"
		}
		body += "```\nAudit history."
		got, err := removeQuestionWorkpadBlocker(body, "owner/repo", "owner/repo#685")
		if err != nil || strings.Contains(got, "ref: owner/repo#685") || !strings.Contains(got, "Audit history.") {
			t.Fatalf("remove = %q, %v", got, err)
		}
		status := "status: in_progress"
		if independent {
			status = "status: blocked"
		}
		if !strings.Contains(got, status) {
			t.Fatalf("park state lost: %s", got)
		}
		again, err := removeQuestionWorkpadBlocker(got, "owner/repo", "owner/repo#685")
		if err != nil || again != got {
			t.Fatalf("retry = %q, %v", again, err)
		}
	}
}

func TestHumanQuestionMigrationRetryRetainsHistory(t *testing.T) {
	t.Parallel()
	for _, partialFailure := range []bool{false, true} {
		tracker := newPrerequisiteTracker(t)
		body := prerequisiteBody(t)
		tracker.issues[10] = restIssue{ID: 10, NodeID: "I_10", Number: 10, State: "open", Body: &body}
		dependent := tracker.issues[1]
		dependentBody := *dependent.Body + "\nDepends on: owner/repo#10\n"
		dependent.Body = &dependentBody
		tracker.issues[1] = dependent
		tracker.edges[1] = []int{9, 10}
		tracker.comments[1] = []restComment{{NodeID: "question", Body: "Use manual delivery?\n" + migrationAuditMarker("owner/repo#10")}}
		hash := sha256.Sum256([]byte(body))
		request := connector.HumanQuestionMigration{Dependent: "owner/repo#1", Source: "owner/repo#10", SourceBodySHA256: hex.EncodeToString(hash[:]), Question: "Use manual delivery?"}
		c := tracker.connector(t)
		if partialFailure {
			tracker.failBody = true
			if err := c.RetireHumanQuestion(t.Context(), request, "question"); err == nil {
				t.Fatal("injected error missing")
			}
			if tracker.issues[10].State != "open" {
				t.Fatal("retired before dependency migration")
			}
		}
		for range 2 {
			if err := c.RetireHumanQuestion(t.Context(), request, "question"); err != nil {
				t.Fatal(err)
			}
		}
		if tracker.creates != 0 || tracker.edgeWrites != 1 {
			t.Fatalf("mutations: creates %d edges %d", tracker.creates, tracker.edgeWrites)
		}
		if tracker.issues[10].State != "closed" || !strings.HasPrefix(*tracker.issues[10].Body, body) || !strings.Contains(*tracker.issues[10].Body, "Superseded") {
			t.Fatalf("retirement = %+v", tracker.issues[10])
		}
		if len(tracker.edges[1]) != 1 || tracker.edges[1][0] != 9 || !strings.Contains(*tracker.issues[1].Body, "#9") {
			t.Fatal("genuine dependency lost")
		}
	}
}
