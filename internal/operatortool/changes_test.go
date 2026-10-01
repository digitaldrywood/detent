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
		{"null binding", BindArtifactService, `{"project_id":"prj_p","binding":null,"request_id":"r"}`, false},
		{"bad source", GetAttemptDiff, `{"project_id":"prj_p","work_item_id":"wi_i","attempt_id":"attempt_a","source":"relay"}`, false},
		{"invalid sequence", GetAttemptDiff, `{"project_id":"prj_p","work_item_id":"wi_i","attempt_id":"attempt_a","sequence":0}`, false},
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

// Catches JSON escaping and long filenames defeating diff page bounds, and
// truncation modifying the cached application source used by other readers.
func TestChangeDiffPages(t *testing.T) {
	for _, test := range []struct {
		name         string
		patch, path  string
		count, limit int
	}{
		{"file pagination", "@@ change", "file.go", 3, 1},
		{"escaped patches", strings.Repeat("\x00", MaxResultBytes/4), "file.go", 2, 200},
		{"long paths", "", strings.Repeat("x", 4096), 200, 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			diff := &tracker.AttemptDiff{ID: "diff_a", AttemptID: "attempt_a", FileCount: test.count}
			for i := 0; i < test.count; i++ {
				diff.Files = append(diff.Files, tracker.AttemptDiffFile{Path: test.path, Patch: test.patch})
			}
			offset := 0
			total := 0
			for {
				page, next := ChangeDiffPage(diff, ChangeArguments{Offset: offset, Limit: test.limit})
				if len(page.Files) == 0 {
					t.Fatal("empty nonterminal page")
				}
				if page.ID != diff.ID || page.FileCount != diff.FileCount {
					t.Fatal("lost source totals/identity")
				}
				if _, err := BoundedChangeResult(ChangeResult{Diff: page}); err != nil {
					t.Fatal(err)
				}
				if test.patch != "" && page.Files[0].Patch == "" && (!page.Files[0].Truncated || !page.Truncated) {
					t.Fatal("silent patch omission")
				}
				total += len(page.Files)
				if next == nil {
					break
				}
				if *next <= offset {
					t.Fatal("pagination did not advance")
				}
				offset = *next
			}
			if total != test.count || diff.Files[0].Patch != test.patch || diff.Truncated {
				t.Fatal("lost files or mutated source")
			}
		})
	}
}
