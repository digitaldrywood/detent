package operatortool

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

type issueContextReader struct {
	failure      error
	largeHistory bool
	fail         string
	malformed    bool
	repeated     bool
	calls        map[string]int
}

func (r *issueContextReader) ReadWork(_ context.Context, name string, request WorkReadRequest) (Result, error) {
	r.calls[name]++
	if name == r.fail {
		return Result{}, r.failure
	}
	if r.malformed {
		return Result{Content: json.RawMessage(`{"data":`)}, nil
	}
	if request.ProjectID != "project" || request.Reference != "issue" || request.Limit != MaxItemLimit {
		return Result{}, ErrInvalidArguments
	}
	if name == WorkHistory && r.largeHistory {
		index, _ := strconv.Atoi(request.Cursor)
		data := map[string]any{"items": []any{map[string]string{"record": "history-" + strconv.Itoa(index), "payload": strings.Repeat("x", 50000)}}}
		if index < 7 {
			data["next_cursor"] = strconv.Itoa(index + 1)
		}
		return EncodeResult(WorkReadResult[any]{Data: data})
	}
	var data any
	switch name {
	case WorkItem:
		data = map[string]string{"body": "entire issue body" + strings.Repeat("x", 5000) + " tail evidence"}
	case WorkRelationships:
		data = map[string]any{"dependencies": []string{"dependency"}, "blockers": []string{"blocker"}}
	case WorkReferences:
		if request.Offset == 0 {
			data = OffsetPage([]string{"change", "PR open"}, 0, 1)
		} else {
			data = OffsetPage([]string{"change", "PR open"}, request.Offset, 1)
		}
	default:
		if request.Cursor == "" || r.repeated {
			item := map[string]string{"record": name + " first", "body": "## Codex Workpad\nold", "created_at": "2026-01-01T00:00:00Z", "comment_id": "old"}
			data = map[string]any{"items": []any{item}, "next_cursor": "second"}
		} else {
			item := map[string]string{"record": name + " second", "body": "## Codex Workpad\nlatest", "created_at": "2026-02-01T00:00:00Z", "comment_id": "latest", "reason": "Needs branch approval", "outcome": "failed"}
			data = map[string]any{"items": []any{item}}
		}
	}
	return EncodeResult(WorkReadResult[any]{Data: data})
}

func TestReadIssueContext(t *testing.T) {
	for _, test := range []struct {
		name, fail          string
		malformed, repeated bool
		want                error
		failure             error
		largeHistory        bool
	}{
		{name: "complete paginated history"},
		{name: "current authority denial", fail: WorkHistory, want: ErrAccessDenied, failure: ErrAccessDenied},
		{name: "unavailable history", fail: WorkHistory, failure: ErrReadUnavailable},
		{name: "oversized history read", fail: WorkHistory, failure: ErrResultTooLarge},
		{name: "bounded aggregate history", largeHistory: true},
		{name: "malformed read", malformed: true},
		{name: "non advancing history", repeated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &issueContextReader{fail: test.fail, failure: test.failure, largeHistory: test.largeHistory, malformed: test.malformed, repeated: test.repeated, calls: map[string]int{}}
			result, err := ReadIssueContext(t.Context(), reader, WorkReadRequest{ProjectID: "project", Reference: "issue"})
			if test.want != nil {
				if !errors.Is(err, test.want) {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if test.malformed || test.repeated {
				if err == nil {
					t.Fatal("invalid pagination accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.fail != "" || test.largeHistory {
				limit, ok := result.Limits[WorkHistory]
				if !ok {
					t.Fatal("missing history evidence limitation")
				}
				encoded, err := json.Marshal(result)
				if err != nil || len(encoded) > MaxResultBytes || len(result.References) != 2 {
					t.Fatalf("invalid bounded context: bytes=%d references=%d err=%v", len(encoded), len(result.References), err)
				}
				if test.fail != "" {
					if limit.Error != test.failure.Error() || len(result.History) != 0 {
						t.Fatalf("failed read presented as complete: %+v", result)
					}
				} else {
					if len(result.History) == 0 || len(result.History) >= 8 || limit.Cursor != strconv.Itoa(len(result.History)) {
						t.Fatalf("lost history continuation: %+v", limit)
					}
					read, err := reader.ReadWork(t.Context(), WorkHistory, WorkReadRequest{ProjectID: "project", Reference: "issue", Limit: MaxItemLimit, Cursor: limit.Cursor})
					if err != nil || !strings.Contains(string(read.Content), "history-"+limit.Cursor) {
						t.Fatalf("continuation skipped evidence: %s %v", read.Content, err)
					}
				}
				return
			}
			if len(result.Comments) != 2 || len(result.History) != 2 || len(result.Attempts) != 2 || len(result.References) != 2 || result.LatestWorkpad == nil || result.LatestWorkpad.ID != "latest" {
				t.Fatalf("incomplete context: %+v", result)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"entire issue body", "tail evidence", "Needs branch approval", "failed", "dependency", "blocker", "PR open", "work_comments second", "work_history second", "work_runs second"} {
				if !strings.Contains(string(encoded), want) {
					t.Fatalf("missing %q in %s", want, encoded)
				}
			}
		})
	}
}
