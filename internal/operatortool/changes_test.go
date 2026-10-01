package operatortool

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// Catches bypasses of wire bounds/required selectors and credential or producer
// authority smuggled into direct calls, independent of client-side schemas.
func TestChangeArgumentBoundary(t *testing.T) {
	for _, test := range []struct {
		name, tool, raw string
		valid           bool
	}{
		{"read", GetChange, `{"project_id":"prj_p","work_item_id":"wi_i","change_id":"change_c"}`, true},
		{"missing selector", GetChange, `{"project_id":"prj_p","work_item_id":"wi_i"}`, false},
		{"forged authority", GetChange, `{"project_id":"prj_p","work_item_id":"wi_i","change_id":"change_c","yolo":true}`, false},
		{"wrong tool field", GetChange, `{"project_id":"prj_p","work_item_id":"wi_i","change_id":"change_c","request_id":"r"}`, false},
		{"worker lease", DiscussChange, `{"project_id":"prj_p","work_item_id":"wi_i","change_id":"change_c","request_id":"r","body":"message","lease_id":"lease"}`, false},
		{"pagination bound", ListChanges, `{"project_id":"prj_p","work_item_id":"wi_i","limit":201}`, false},
		{"null selector", GetChange, `{"project_id":"prj_p","work_item_id":"wi_i","change_id":null}`, false},
		{"path injection", GetChange, `{"project_id":"prj_p","work_item_id":"wi_i","change_id":"change_c/foreign"}`, false},
		{"oversized body", DiscussChange, `{"project_id":"prj_p","work_item_id":"wi_i","change_id":"change_c","request_id":"r","body":"` + strings.Repeat("x", 32769) + `"}`, false},
		{"forged bundle field", ViewChangeFile, `{"project_id":"prj_p","work_item_id":"wi_i","change_id":"change_c","version_id":"version_v","request_id":"r","viewed":false,"file_sha256":"` + strings.Repeat("a", 64) + `","bundle":{"artifact_id":"artifact_a","revision":1,"sha256":"` + strings.Repeat("a", 64) + `","head_sha":"` + strings.Repeat("b", 40) + `","token":"forged"}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeChangeArguments(test.tool, json.RawMessage(test.raw))
			if (err == nil) != test.valid {
				t.Fatalf("decode=%v valid=%v", err, test.valid)
			}
		})
	}
}

// Catches an oversized application response escaping the shared transport cap.
func TestChangeResultBound(t *testing.T) {
	value := ChangeResult{Detail: &tracker.ChangeDetail{Change: tracker.ChangeRequest{Body: strings.Repeat("x", MaxResultBytes)}}}
	if _, err := BoundedChangeResult(value); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("oversized response: %v", err)
	}
}
