package operatortool

import (
	"encoding/json"
	"strings"
	"testing"
)

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
