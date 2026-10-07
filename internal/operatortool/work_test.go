package operatortool

import (
	"encoding/json"
	"errors"
	"math"
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
		{"reported issue comment", AddComment, `"body":"` + strings.Repeat("Discuss `approval` at /chat/approval. ", 25) + `","target":"issue"`, true},
		{"ordinary comment", AddComment, `"body":"hello"`, true},
		{"PR comment", AddComment, `"body":"hello","target":"pr","repository":"owner/repo","pull_request":1`, true},
		{"missing PR ownership", AddComment, `"body":"hello","target":"pr"`, false},
		{"irrelevant PR target", AddComment, `"body":"hello","repository":"owner/repo"`, false},
		{"forged approval", AddComment, `"body":"hello","confirm":true`, false},
		{"forged YOLO", AddComment, `"body":"hello","yolo":true`, false},
		{"field from another command", AddComment, `"body":"hello","labels":[]`, false},
		{"null comment", AddComment, `"body":null`, false},
		{"oversized comment", AddComment, `"body":"` + strings.Repeat("x", 32769) + `"`, false},
		{"empty edit", EditItem, `"expected_revision":"1"`, false},
		{"revision required", EditItem, `"title":"title"`, false},
		{"zero revision", EditItem, `"title":"title","expected_revision":"0"`, false},
		{"invalid priority", EditItem, `"priority":4,"expected_revision":"1"`, false},
		{"numeric edit revision", EditItem, `"priority":0,"expected_revision":1`, false},
		{"ordinary edit", EditItem, `"priority":0,"expected_revision":"1"`, true},
		{"native full title", EditItem, `"title":"` + strings.Repeat("x", 500) + `","expected_revision":"1"`, true},
		{"native oversized title", EditItem, `"title":"` + strings.Repeat("x", 501) + `","expected_revision":"1"`, false},
		{"blank label", EditItem, `"labels":[" "],"expected_revision":"1"`, false},
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
		revision     int64
	}{
		{"configured state", `"target_state":"Ready","expected_revision":"2"`, true, 2},
		{"response revision", `"target_state":"Ready","expected_revision":"1"`, true, 1},
		{"maximum revision", `"target_state":"Ready","expected_revision":"9223372036854775807"`, true, math.MaxInt64},
		{"numeric revision", `"target_state":"Ready","expected_revision":1`, false, 0},
		{"overflow revision", `"target_state":"Ready","expected_revision":"9223372036854775808"`, false, 0},
		{"negative revision", `"target_state":"Ready","expected_revision":"-1"`, false, 0},
		{"signed revision", `"target_state":"Ready","expected_revision":"+1"`, false, 0},
		{"fractional revision", `"target_state":"Ready","expected_revision":"1.5"`, false, 0},
		{"nondecimal revision", `"target_state":"Ready","expected_revision":"1e2"`, false, 0},
		{"blank revision", `"target_state":"Ready","expected_revision":""`, false, 0},
		{"padded revision", `"target_state":"Ready","expected_revision":" 1 "`, false, 0},
		{"leading zero", `"target_state":"Ready","expected_revision":"01"`, false, 0},
		{"null revision", `"target_state":"Ready","expected_revision":null`, false, 0},
		{"missing revision", `"target_state":"Ready"`, false, 0},
		{"zero revision", `"target_state":"Ready","expected_revision":"0"`, false, 0},
		{"blank state", `"target_state":" ","expected_revision":"2"`, false, 0},
		{"oversized state", `"target_state":"` + strings.Repeat("x", 257) + `","expected_revision":"2"`, false, 0},
		{"forged approval", `"target_state":"Ready","expected_revision":"2","confirm":true`, false, 0},
		{"forged fence", `"target_state":"Ready","expected_revision":"2","fencing_token":12`, false, 0},
		{"forged reason", `"target_state":"Ready","expected_revision":"2","reason":"worker_progress"`, false, 0},
		{"null field", `"target_state":null,"expected_revision":"2"`, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := DecodeNativeMoveItem(json.RawMessage(`{"project_id":"project","request_id":"move","identifier":"item",` + test.fields + `}`))
			if (err == nil) != test.valid {
				t.Fatalf("valid=%t error=%v", test.valid, err)
			}
			if err == nil && request.ExpectedRevision != test.revision {
				t.Fatalf("revision=%d want=%d", request.ExpectedRevision, test.revision)
			}
		})
	}
}

func TestToolArgumentValidationDetails(t *testing.T) {
	for _, test := range []struct {
		name, tool, raw, message string
	}{
		{"move missing revision", MoveItem, `{"project_id":"detent","request_id":"move","identifier":"295","target_state":"Done"}`, "expected_revision: is required"},
		{"move rejected revision", MoveItem, `{"project_id":"detent","request_id":"move","identifier":"329","target_state":"Todo","expected_revision":"0"}`, "expected_revision: must match pattern ^[1-9][0-9]*$"},
		{"comment missing body", AddComment, `{"project_id":"detent","identifier":"259"}`, "body: is required"},
		{"comment rejected target", AddComment, `{"project_id":"detent","identifier":"259","body":"hello","target":"other"}`, "target: must be one of: issue, pr"},
		{"comment rejected body", AddComment, `{"project_id":"detent","identifier":"259","body":"` + strings.Repeat("x", 32769) + `"}`, "body: must not exceed 32768 bytes"},
		{"comment blank body", AddComment, `{"project_id":"detent","identifier":"259","body":" "}`, "body: must not be blank"},
		{"comment missing PR repository", AddComment, `{"project_id":"detent","identifier":"259","body":"hello","target":"pr","pull_request":1}`, "repository: is required when target is pr"},
		{"comment missing PR number", AddComment, `{"project_id":"detent","identifier":"259","body":"hello","target":"pr","repository":"owner/repo"}`, "pull_request: is required when target is pr"},
		{"comment irrelevant PR repository", AddComment, `{"project_id":"detent","identifier":"259","body":"hello","target":"issue","repository":"owner/repo"}`, "repository: is only allowed when target is pr"},
		{"comment unknown field", AddComment, `{"project_id":"detent","identifier":"259","body":"hello","confirm":true}`, "confirm: is not an allowed field"},
		{"comment wrong body type", AddComment, `{"project_id":"detent","identifier":"259","body":123}`, "body: must have type string"},
		{"comment null body", AddComment, `{"project_id":"detent","identifier":"259","body":null}`, "body: must be a string"},
		{"edit missing revision", EditItem, `{"project_id":"detent","identifier":"259","title":"title"}`, "expected_revision: is required"},
		{"edit rejected label", EditItem, `{"project_id":"detent","identifier":"259","expected_revision":"1","labels":[""]}`, "labels[0]: must contain at least 1 characters"},
		{"comments rejected limit", ListComments, `{"project_id":"detent","identifier":"259","limit":201}`, "limit: must be at most 200"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var err error
			if test.tool == MoveItem {
				_, err = DecodeNativeMoveItem(json.RawMessage(test.raw))
			} else {
				_, err = DecodeWorkArguments(test.tool, json.RawMessage(test.raw))
			}
			var detail *RequestError
			if !errors.Is(err, ErrInvalidArguments) || !errors.As(err, &detail) {
				t.Fatalf("error = %v, want structured invalid arguments", err)
			}
			if detail.Code != "invalid_request" || detail.Message != test.message {
				t.Fatalf("detail = %+v, want invalid_request with %q", detail, test.message)
			}
		})
	}
}
