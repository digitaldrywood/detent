package selector

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestMatch(t *testing.T) {
	t.Parallel()

	issue := connector.Issue{
		AuthorID:   "corylanou",
		AssigneeID: "worker-1",
		Assignees:  []string{"worker-1", "worker-2"},
		Labels:     []string{"Enhancement", "stage:s2"},
		Fields: map[string]string{
			"Status": "Todo",
			"Track":  "multi-instance",
		},
	}
	ctx := Context{
		InstanceLogin: "worker-2",
		Persona:       "persona-reviewer",
	}

	tests := []struct {
		name     string
		issue    *connector.Issue
		ctx      *Context
		selector Selector
		want     bool
	}{
		{
			name: "assignee in matches any assigned login",
			selector: Selector{
				AssigneeIn: []string{"worker-2"},
			},
			want: true,
		},
		{
			name: "assignee in misses unassigned login",
			selector: Selector{
				AssigneeIn: []string{"worker-3"},
			},
			want: false,
		},
		{
			name: "assignee in matches legacy assignee id",
			issue: &connector.Issue{
				AssigneeID: "worker-legacy",
				Labels:     []string{},
				Fields:     map[string]string{},
			},
			selector: Selector{
				AssigneeIn: []string{"worker-legacy"},
			},
			want: true,
		},
		{
			name: "author in matches author login",
			selector: Selector{
				AuthorIn: []string{"corylanou"},
			},
			want: true,
		},
		{
			name: "label include requires every requested label",
			selector: Selector{
				Labels: Labels{
					Include: []string{"enhancement", "stage:s2"},
				},
			},
			want: true,
		},
		{
			name: "label include misses absent label",
			selector: Selector{
				Labels: Labels{
					Include: []string{"bug"},
				},
			},
			want: false,
		},
		{
			name: "label exclude rejects present label",
			selector: Selector{
				Labels: Labels{
					Exclude: []string{"enhancement"},
				},
			},
			want: false,
		},
		{
			name: "label exclude allows absent label",
			selector: Selector{
				Labels: Labels{
					Exclude: []string{"bug"},
				},
			},
			want: true,
		},
		{
			name: "field equals matches configured project field",
			selector: Selector{
				Fields: []FieldEquals{
					{Name: "Track", Value: "multi-instance"},
				},
			},
			want: true,
		},
		{
			name: "field equals misses different project field value",
			selector: Selector{
				Fields: []FieldEquals{
					{Name: "Status", Value: "In Progress"},
				},
			},
			want: false,
		},
		{
			name: "priority in matches configured rank",
			issue: &connector.Issue{
				Priority: intPtr(2),
				Labels:   []string{},
				Fields:   map[string]string{},
			},
			selector: Selector{
				PriorityIn: []int{1, 2},
			},
			want: true,
		},
		{
			name: "priority in rejects different rank",
			issue: &connector.Issue{
				Priority: intPtr(3),
				Labels:   []string{},
				Fields:   map[string]string{},
			},
			selector: Selector{
				PriorityIn: []int{1, 2},
			},
			want: false,
		},
		{
			name: "and group requires every child selector",
			selector: Selector{
				And: []Selector{
					{AuthorIn: []string{"corylanou"}},
					{Labels: Labels{Include: []string{"stage:s2"}}},
				},
			},
			want: true,
		},
		{
			name: "and group rejects false child selector",
			selector: Selector{
				And: []Selector{
					{AuthorIn: []string{"corylanou"}},
					{Labels: Labels{Include: []string{"bug"}}},
				},
			},
			want: false,
		},
		{
			name: "or group matches any child selector",
			selector: Selector{
				Or: []Selector{
					{Labels: Labels{Include: []string{"bug"}}},
					{Fields: []FieldEquals{{Name: "Status", Value: "Todo"}}},
				},
			},
			want: true,
		},
		{
			name: "or group rejects when no child selector matches",
			selector: Selector{
				Or: []Selector{
					{Labels: Labels{Include: []string{"bug"}}},
					{Fields: []FieldEquals{{Name: "Status", Value: "Done"}}},
				},
			},
			want: false,
		},
		{
			name: "at me matches instance login",
			selector: Selector{
				AssigneeIn: []string{"@me"},
			},
			want: true,
		},
		{
			name: "at me misses when neither identity matches author",
			selector: Selector{
				AuthorIn: []string{"@me"},
			},
			want: false,
		},
		{
			name: "at me matches configured persona",
			issue: &connector.Issue{
				AuthorID:  "persona-reviewer",
				Labels:    []string{},
				Fields:    map[string]string{},
				Assignees: []string{},
			},
			selector: Selector{
				AuthorIn: []string{"@me"},
			},
			want: true,
		},
		{
			name: "empty selector matches",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			testIssue := issue
			if tt.issue != nil {
				testIssue = *tt.issue
			}
			testCtx := ctx
			if tt.ctx != nil {
				testCtx = *tt.ctx
			}

			if got := Match(testIssue, tt.selector, testCtx); got != tt.want {
				t.Fatalf("Match() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestDescribe(t *testing.T) {
	t.Parallel()

	ctx := Context{
		InstanceLogin: "worker-1",
		Persona:       "release-captain",
	}

	tests := []struct {
		name     string
		selector Selector
		want     string
	}{
		{
			name: "empty selector describes all issues",
			want: "All issues",
		},
		{
			name: "selector describes configured filters",
			selector: Selector{
				AssigneeIn: []string{"@me", "backup"},
				Labels: Labels{
					Include: []string{"release"},
					Exclude: []string{"blocked"},
				},
				Fields: []FieldEquals{
					{Name: "Owner", Value: "@me"},
				},
				PriorityIn: []int{1, 3},
			},
			want: "assignee in @me (worker-1, release-captain), backup; labels include release; labels exclude blocked; field Owner = @me (worker-1, release-captain); priority in 1, 3",
		},
		{
			name: "selector describes combined children",
			selector: Selector{
				And: []Selector{
					{AssigneeIn: []string{"@me"}},
					{Labels: Labels{Include: []string{"multi-instance"}}},
				},
			},
			want: "all (assignee in @me (worker-1, release-captain); labels include multi-instance)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := Describe(tt.selector, ctx); got != tt.want {
				t.Fatalf("Describe() = %q, want %q", got, tt.want)
			}
		})
	}
}

func intPtr(value int) *int {
	return &value
}
