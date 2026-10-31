package selector

import (
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestNativeFilterHint(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		auth   Selector
		ctx    Context
		want   connector.IssueFilterHint
		refuse bool
	}{
		{name: "unrestricted"},
		{name: "include labels", auth: Selector{Labels: Labels{Include: []string{"ready"}}}, want: connector.IssueFilterHint{LabelInclude: []string{"ready"}}},
		{name: "exclude labels", auth: Selector{Labels: Labels{Exclude: []string{"manual"}}}, want: connector.IssueFilterHint{LabelExclude: []string{"manual"}}},
		{name: "blank required label refuses", auth: Selector{Labels: Labels{Include: []string{" "}}}, refuse: true},
		{name: "blank excluded label refuses", auth: Selector{Labels: Labels{Exclude: []string{" "}}}, refuse: true},
		{name: "host and workflow labels", auth: Selector{And: []Selector{{Labels: Labels{Include: []string{"Ready"}, Exclude: []string{"manual"}}}, {And: []Selector{{Labels: Labels{Include: []string{"canary"}}}}}}}, want: connector.IssueFilterHint{LabelInclude: []string{"ready", "canary"}, LabelExclude: []string{"manual"}}},
		{name: "single alternative", auth: Selector{Or: []Selector{{Labels: Labels{Include: []string{"ready"}}}}}, want: connector.IssueFilterHint{LabelInclude: []string{"ready"}}},
		{name: "nested alternatives refuse", auth: Selector{And: []Selector{{Or: []Selector{{Labels: Labels{Include: []string{"a"}}}, {Labels: Labels{Include: []string{"b"}}}}}}}, refuse: true},
		{name: "priority refuses", auth: Selector{PriorityIn: []int{1}}, refuse: true},
		{name: "fields refuse", auth: Selector{Fields: []FieldEquals{{Name: "team", Value: "a"}}}, refuse: true},
		{name: "unresolved identity refuses", auth: Selector{AuthorIn: []string{"@me"}}, refuse: true},
		{name: "resolved identity", auth: Selector{AuthorIn: []string{"@me"}}, ctx: Context{InstanceLogin: "Alice"}, want: connector.IssueFilterHint{Authors: []string{"alice"}}},
		{name: "author intersection", auth: Selector{AuthorIn: []string{"alice", "bob"}, And: []Selector{{AuthorIn: []string{"bob", "carol"}}}}, want: connector.IssueFilterHint{Authors: []string{"bob"}}},
		{name: "impossible author refuses", auth: Selector{AuthorIn: []string{"alice"}, And: []Selector{{AuthorIn: []string{"bob"}}}}, refuse: true},
		{name: "assignee subset", auth: Selector{AssigneeIn: []string{"alice", "bob"}, And: []Selector{{AssigneeIn: []string{"bob"}}}}, want: connector.IssueFilterHint{Assignees: []string{"bob"}}},
		{name: "multiple assignee clauses refuse", auth: Selector{AssigneeIn: []string{"alice", "bob"}, And: []Selector{{AssigneeIn: []string{"bob", "carol"}}}}, refuse: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NativeFilterHint(tt.auth, tt.ctx)
			if tt.refuse {
				if err == nil {
					t.Fatal("unsupported selector was admitted")
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("hint=%#v error=%v want=%#v", got, err, tt.want)
			}
			flat := Selector{AuthorIn: got.Authors, AssigneeIn: got.Assignees, Labels: Labels{Include: got.LabelInclude, Exclude: got.LabelExclude}}
			for _, issue := range []connector.Issue{{}, {Labels: []string{"ready"}}, {Labels: []string{"ready", "canary"}}, {Labels: []string{"ready", "canary", "manual"}}, {AuthorID: "bob", Assignees: []string{"alice", "carol"}}, {AuthorID: "alice", Assignees: []string{"bob"}}} {
				if Match(issue, tt.auth, tt.ctx) != Match(issue, flat, Context{}) {
					t.Fatalf("conversion changed predicate for %#v", issue)
				}
			}
		})
	}
}
