package hubserver

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// The queue's priority is a field an operator can take off again, not only
// move between levels. "none" is the word that says so; every other word
// still names a level, and the mirrored `priority:` label follows either way.
func TestChangeWorkItemPriorityClears(t *testing.T) {
	t.Parallel()
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	_, issueID := seedProjection(t, service.database.db)
	seedHubAPIToken(t, service, "operator-token", "operator-secret-token", apiScopeOperator)
	path := fmt.Sprintf("/api/v1/work-items/%d/priority", issueID)

	readPriority := func(t *testing.T) (int64, bool) {
		t.Helper()
		var priority sql.NullInt64
		if err := service.database.db.QueryRowContext(t.Context(), "SELECT priority_override FROM queue_entries WHERE issue_id = ? AND scope = 'fleet'", issueID).Scan(&priority); err != nil {
			t.Fatalf("read queue entry: %v", err)
		}
		return priority.Int64, priority.Valid
	}
	readDesired := func(t *testing.T, key string) WorkflowLabelDesired {
		t.Helper()
		var encoded string
		if err := service.database.db.QueryRowContext(t.Context(), "SELECT desired_json FROM github_outbox WHERE idempotency_key = ?", key).Scan(&encoded); err != nil {
			t.Fatalf("read outbox record %q: %v", key, err)
		}
		var desired WorkflowLabelDesired
		if err := json.Unmarshal([]byte(encoded), &desired); err != nil {
			t.Fatalf("decode outbox record %q: %v", key, err)
		}
		return desired
	}

	for _, test := range []struct {
		name     string
		priority string
		key      string
		status   int
		want     int64
		valid    bool
		label    string
		clear    bool
	}{
		{name: "set", priority: "high", key: "priority-set", status: http.StatusAccepted, want: tracker.QueuePriorityHigh, valid: true, label: "priority:high"},
		{name: "clear", priority: "none", key: "priority-clear", status: http.StatusAccepted, clear: true},
		{name: "clear twice", priority: "none", key: "priority-clear-again", status: http.StatusAccepted, clear: true},
		{name: "set again", priority: "urgent", key: "priority-reset", status: http.StatusAccepted, want: tracker.QueuePriorityUrgent, valid: true, label: "priority:urgent"},
		{name: "nonsense", priority: "highest", key: "priority-bad", status: http.StatusUnprocessableEntity, want: tracker.QueuePriorityUrgent, valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, service, http.MethodPost, path, "operator-secret-token", map[string]any{
				"scope": "fleet", "state": "In Progress", "priority": test.priority, "idempotency_key": test.key,
			})
			if response.Code != test.status {
				t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
			}
			value, valid := readPriority(t)
			if valid != test.valid || (valid && value != test.want) {
				t.Fatalf("priority_override = (%d, %v), want (%d, %v)", value, valid, test.want, test.valid)
			}
			if test.status != http.StatusAccepted {
				return
			}
			desired := readDesired(t, test.key)
			if desired.Clear != test.clear || desired.Label != test.label {
				t.Fatalf("outbox desired = %#v, want clear %v label %q", desired, test.clear, test.label)
			}
			if desired.ManagedPrefix != "priority:" {
				t.Fatalf("managed prefix = %q", desired.ManagedPrefix)
			}
		})
	}
}

// The outbox record is the contract between the hub and the GitHub writer, so
// a clear has to survive the round trip that stores it.
func TestWorkflowLabelOutboxRecordAcceptsAClear(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		mutation WorkflowLabelMutation
		wantErr  bool
	}{
		{
			name:     "a label",
			mutation: WorkflowLabelMutation{IdempotencyKey: "k", RepositoryID: 1, IssueID: 2, Label: "priority:high", ManagedPrefix: "priority:"},
		},
		{
			name:     "a clear",
			mutation: WorkflowLabelMutation{IdempotencyKey: "k", RepositoryID: 1, IssueID: 2, ManagedPrefix: "priority:", Clear: true},
		},
		{
			name:     "neither",
			mutation: WorkflowLabelMutation{IdempotencyKey: "k", RepositoryID: 1, IssueID: 2, ManagedPrefix: "priority:"},
			wantErr:  true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, err := test.mutation.outboxRecord()
			if (err != nil) != test.wantErr {
				t.Fatalf("outboxRecord() error = %v, want error %v", err, test.wantErr)
			}
			if test.wantErr {
				return
			}
			var desired WorkflowLabelDesired
			if err := json.Unmarshal(record.desired, &desired); err != nil {
				t.Fatal(err)
			}
			if desired.Clear != test.mutation.Clear || desired.Label != test.mutation.Label {
				t.Fatalf("desired = %#v, want %#v", desired, test.mutation)
			}
		})
	}
}
