package operatortool

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkItemResultContentBounds(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		body      string
		comments  string
		labels    []string
		truncated bool
		tooLarge  bool
		fields    []string
	}{
		{name: "normal", body: "full body", comments: "full comment"},
		{name: "large but within limit", body: strings.Repeat("x", 200000)},
		{name: "oversized body and comments", body: strings.Repeat("界", 100001), comments: strings.Repeat("c", 300001), truncated: true, fields: []string{"body", "linked_source.snapshot.body", "linked_source.snapshot.comments"}},
		{name: "escaped body", body: strings.Repeat("\x00", 50000), truncated: true, fields: []string{"body", "linked_source.snapshot.body"}},
		{name: "comments only", comments: strings.Repeat("c", 300001), truncated: true, fields: []string{"linked_source.snapshot.comments"}},
		{name: "required metadata too large", labels: []string{strings.Repeat("l", MaxResultBytes)}, tooLarge: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.labels == nil {
				test.labels = []string{"bug"}
			}
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_example", Revision: 7}, Body: test.body, State: "Todo", Labels: test.labels, LinkedSource: &tracker.LinkedIssueSource{URL: "https://github.com/owner/repository/issues/10", Status: "linked", Snapshot: &tracker.GitHubIssueSnapshot{Body: test.body, Comments: []tracker.GitHubIssueComment{{Body: test.comments}}}}}
			if !test.truncated {
				issue.LinkedSource.Snapshot.Body = "snapshot body"
			}
			view := WorkReadResult[NativeItem]{Data: NativeItemView("prj_example", issue)}
			full, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			result, err := EncodeWorkItemResult(view)
			if test.tooLarge {
				var detail *ResultTooLargeError
				if !errors.As(err, &detail) || !errors.Is(err, ErrResultTooLarge) || detail.SizeBytes <= MaxResultBytes || detail.LimitBytes != MaxResultBytes {
					t.Fatalf("oversize error = %v", err)
				}
				return
			}
			if err != nil || len(result.Content) > MaxResultBytes {
				t.Fatalf("result bytes=%d error=%v", len(result.Content), err)
			}
			if !test.truncated {
				if string(result.Content) != string(full) {
					t.Fatal("normal item shape or content changed")
				}
				return
			}
			var output struct {
				Data struct {
					tracker.NativeIssue
					ContentRead struct {
						TruncatedFields []string        `json:"truncated_fields"`
						FullURL         string          `json:"full_url"`
						CommentsTool    string          `json:"comments_tool"`
						Arguments       WorkReadRequest `json:"arguments"`
					} `json:"content_read"`
				} `json:"data"`
			}
			if err := json.Unmarshal(result.Content, &output); err != nil {
				t.Fatal(err)
			}
			got := output.Data
			if !reflect.DeepEqual(got.ContentRead.TruncatedFields, test.fields) || got.ContentRead.Arguments.Limit != 1 {
				t.Fatalf("continuation fields=%v arguments=%+v", got.ContentRead.TruncatedFields, got.ContentRead.Arguments)
			}
			if got.Revision != 7 || got.State != "Todo" || got.WorkItemID != "wi_example" || !reflect.DeepEqual(got.Labels, test.labels) || !utf8.ValidString(got.Body) || !strings.HasPrefix(test.body, got.Body) || len(got.ContentRead.TruncatedFields) == 0 || got.ContentRead.FullURL != "/api/v2/organizations/org_example/projects/prj_example/work-items/wi_example" || got.ContentRead.CommentsTool != WorkComments || got.ContentRead.Arguments.ProjectID != "prj_example" || got.ContentRead.Arguments.Reference != "wi_example" {
				t.Fatalf("bounded metadata = %+v", got.ContentRead)
			}
			if len(test.comments) > 4096 && len(got.LinkedSource.Snapshot.Comments) != 0 {
				t.Fatal("oversized snapshot comments were retained")
			}
			if issue.Body != test.body || issue.LinkedSource.Snapshot.Body != test.body || issue.LinkedSource.Snapshot.Comments[0].Body != test.comments {
				t.Fatal("MCP shaping mutated the source item")
			}
		})
	}
}
