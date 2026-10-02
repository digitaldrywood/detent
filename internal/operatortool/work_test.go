package operatortool

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFileIssueArguments(t *testing.T) {
	for _, tt := range []struct {
		name, fields string
		valid        bool
	}{
		{"ordinary", `"title":"Title","description":"Body"`, true},
		{"linked only", `"github_issue_url":"github.com/owner/repo/issues/1"`, true},
		{"linked priority", `"github_issue_url":"github.com/owner/repo/issues/1","priority":4`, true},
		{"native empty body", `"title":"Title"`, true},
		{"native full title", `"title":"` + strings.Repeat("x", 500) + `"`, true},
		{"native oversized title", `"title":"` + strings.Repeat("x", 501) + `"`, false},
		{"missing title", `"description":"Body"`, false},
		{"blank link", `"github_issue_url":" "`, false},
		{"null link", `"github_issue_url":null`, false},
		{"credential URL", `"github_issue_url":"https://user:secret@github.com/owner/repo/issues/1"`, false},
		{"PR instead of issue", `"github_issue_url":"https://github.com/owner/repo/pull/1"`, false},
		{"oversized link", `"github_issue_url":"` + strings.Repeat("x", 2049) + `"`, false},
		{"wrong priority scale", `"title":"Title","description":"Body","priority":0`, false},
		{"forged approval", `"title":"Title","description":"Body","confirm":true`, false},
		{"forged authority", `"github_issue_url":"github.com/owner/repo/issues/1","organization_id":"other"`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeFileIssue(json.RawMessage(`{"project_id":"project",` + tt.fields + `}`))
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%t error=%v", tt.valid, err)
			}
		})
	}
}

// Direct callers must not smuggle authority, another command's fields, or
// unbounded content past a schema that was only used during discovery.
func TestWorkArgumentsDirectCallBounds(t *testing.T) {
	for _, tt := range []struct {
		name, tool, fields string
		valid              bool
	}{
		{"ordinary comment", AddComment, `"body":"hello"`, true},
		{"PR comment", AddComment, `"body":"hello","target":"pr","repository":"owner/repo","pull_request":1`, true},
		{"missing PR ownership", AddComment, `"body":"hello","target":"pr"`, false},
		{"irrelevant PR target", AddComment, `"body":"hello","repository":"owner/repo"`, false},
		{"forged approval", AddComment, `"body":"hello","confirm":true`, false},
		{"forged YOLO", AddComment, `"body":"hello","yolo":true`, false},
		{"field from another command", AddComment, `"body":"hello","labels":[]`, false},
		{"null comment", AddComment, `"body":null`, false},
		{"oversized comment", AddComment, `"body":"` + strings.Repeat("x", 32769) + `"`, false},
		{"empty edit", EditItem, `"expected_revision":1`, false},
		{"revision required", EditItem, `"title":"title"`, false},
		{"zero revision", EditItem, `"title":"title","expected_revision":0`, false},
		{"invalid priority", EditItem, `"priority":4,"expected_revision":1`, false},
		{"ordinary edit", EditItem, `"priority":0,"expected_revision":1`, true},
		{"native full title", EditItem, `"title":"` + strings.Repeat("x", 500) + `","expected_revision":1`, true},
		{"native oversized title", EditItem, `"title":"` + strings.Repeat("x", 501) + `","expected_revision":1`, false},
		{"blank label", EditItem, `"labels":[" "],"expected_revision":1`, false},
		{"unknown dependency operation", SetDependency, `"related":"item","operation":"replace"`, false},
		{"invalid queue priority", SetQueuePriority, `"queue_scope":"work","state":"Todo","queue_priority":"highest"`, false},
		{"oversized read", ListComments, `"limit":201`, false},
		{"bounded read", ListComments, `"limit":1`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeWorkArguments(tt.tool, json.RawMessage(`{"project_id":"project","identifier":"item",`+tt.fields+`}`))
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%t error=%v", tt.valid, err)
			}
		})
	}
}

func TestNativeMoveItemArguments(t *testing.T) {
	for _, test := range []struct {
		name, fields string
		valid        bool
	}{
		{"configured state", `"target_state":"Ready","expected_revision":2`, true},
		{"missing revision", `"target_state":"Ready"`, false},
		{"zero revision", `"target_state":"Ready","expected_revision":0`, false},
		{"blank state", `"target_state":" ","expected_revision":2`, false},
		{"oversized state", `"target_state":"` + strings.Repeat("x", 257) + `","expected_revision":2`, false},
		{"forged approval", `"target_state":"Ready","expected_revision":2,"confirm":true`, false},
		{"forged fence", `"target_state":"Ready","expected_revision":2,"fencing_token":12`, false},
		{"forged reason", `"target_state":"Ready","expected_revision":2,"reason":"worker_progress"`, false},
		{"null field", `"target_state":null,"expected_revision":2`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeNativeMoveItem(json.RawMessage(`{"project_id":"project","request_id":"move","identifier":"item",` + test.fields + `}`))
			if (err == nil) != test.valid {
				t.Fatalf("valid=%t error=%v", test.valid, err)
			}
		})
	}
}
