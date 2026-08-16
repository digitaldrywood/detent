package connector

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestCandidateCapabilitiesFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		backend           Backend
		statusSource      string
		states            bool
		labels            bool
		untracked         bool
		authorAssociation bool
		authorPushdown    bool
	}{
		{name: "github project v2", backend: BackendGitHub, statusSource: "project_v2", states: true, authorAssociation: true},
		{name: "github issue field", backend: BackendGitHub, statusSource: "issue_field", states: true, labels: true, authorAssociation: true, authorPushdown: true},
		{name: "github label", backend: BackendGitHub, statusSource: "label", states: true, labels: true, untracked: true, authorAssociation: true},
		{name: "github default", backend: BackendGitHub, states: true, authorAssociation: true},
		{name: "github invalid source", backend: BackendGitHub, statusSource: "milestone"},
		{name: "github local", backend: BackendGitHubLocal, states: true, labels: true, authorAssociation: true},
		{name: "local sqlite", backend: BackendLocalSQLite, states: true, labels: true},
		{name: "memory", backend: BackendMemory, states: true, labels: true},
		{name: "linear", backend: BackendLinear},
		{name: "gitlab", backend: BackendGitLab},
		{name: "jira", backend: BackendJira},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			capabilities := CandidateCapabilitiesFor(test.backend, test.statusSource)
			if got := capabilities.Supports(CandidateSelectorStates); got != test.states {
				t.Fatalf("Supports(states) = %t, want %t", got, test.states)
			}
			if got := capabilities.Supports(CandidateSelectorLabels); got != test.labels {
				t.Fatalf("Supports(labels) = %t, want %t", got, test.labels)
			}
			if got := capabilities.Supports(CandidateSelectorUntracked); got != test.untracked {
				t.Fatalf("Supports(untracked) = %t, want %t", got, test.untracked)
			}
			if capabilities.AuthorAssociation != test.authorAssociation {
				t.Fatalf("AuthorAssociation = %t, want %t", capabilities.AuthorAssociation, test.authorAssociation)
			}
			if got := capabilities.SupportsPushdown(CandidateFilterAuthorHandle); got != test.authorPushdown {
				t.Fatalf("SupportsPushdown(author_handle) = %t, want %t", got, test.authorPushdown)
			}
		})
	}
}

func TestCandidateRequestValidate(t *testing.T) {
	t.Parallel()

	capabilities := CandidateCapabilitiesFor(BackendGitHub, "label")
	tests := []struct {
		name    string
		request CandidateRequest
		want    error
	}{
		{
			name: "unsupported author pushdown",
			request: CandidateRequest{
				Selector: CandidateSelectorStates,
				States:   []string{"Backlog"},
				Authors:  []string{"octocat"},
				Limit:    10,
			},
			want: ErrCandidateFilterUnsupported,
		},
		{
			name: "valid states selector",
			request: CandidateRequest{
				Selector: CandidateSelectorStates,
				States:   []string{" Backlog ", "backlog"},
				Limit:    10,
			},
		},
		{
			name: "valid labels selector",
			request: CandidateRequest{
				Selector: CandidateSelectorLabels,
				Labels:   []string{" Sentry ", "sentry"},
				Limit:    10,
			},
		},
		{
			name: "valid untracked selector",
			request: CandidateRequest{
				Selector: CandidateSelectorUntracked,
				Limit:    10,
			},
		},
		{
			name: "unsupported selector",
			request: CandidateRequest{
				Selector: CandidateSelector("authors"),
				Limit:    10,
			},
			want: ErrCandidateSelectorUnsupported,
		},
		{
			name: "non-positive limit",
			request: CandidateRequest{
				Selector: CandidateSelectorStates,
				States:   []string{"Backlog"},
			},
			want: ErrInvalidCandidateRequest,
		},
		{
			name: "negative page size",
			request: CandidateRequest{
				Selector: CandidateSelectorStates,
				States:   []string{"Backlog"},
				Limit:    10,
				PageSize: -1,
			},
			want: ErrInvalidCandidateRequest,
		},
		{
			name: "empty states",
			request: CandidateRequest{
				Selector: CandidateSelectorStates,
				States:   []string{"  "},
				Limit:    10,
			},
			want: ErrInvalidCandidateRequest,
		},
		{
			name: "empty labels",
			request: CandidateRequest{
				Selector: CandidateSelectorLabels,
				Labels:   []string{"  "},
				Limit:    10,
			},
			want: ErrInvalidCandidateRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.request.Validate(capabilities)
			if !errors.Is(err, test.want) {
				t.Fatalf("Validate() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCandidateRequestRejectsAuthorPushdownForNonStateSelector(t *testing.T) {
	t.Parallel()

	request := CandidateRequest{
		Selector: CandidateSelectorLabels,
		Labels:   []string{"sentry"},
		Authors:  []string{"octocat"},
		Limit:    10,
	}
	err := request.Validate(CandidateCapabilitiesFor(BackendGitHub, "issue_field"))
	if !errors.Is(err, ErrCandidateFilterUnsupported) {
		t.Fatalf("Validate() error = %v, want %v", err, ErrCandidateFilterUnsupported)
	}
}

func TestAuthorAssociationValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		want  AuthorAssociation
		valid bool
	}{
		{value: " owner ", want: AuthorAssociationOwner, valid: true},
		{value: "MEMBER", want: AuthorAssociationMember, valid: true},
		{value: "collaborator", want: AuthorAssociationCollaborator, valid: true},
		{value: "contributor", want: AuthorAssociationContributor, valid: true},
		{value: "first_time_contributor", want: AuthorAssociationFirstTimeContributor, valid: true},
		{value: "none", want: AuthorAssociationNone, valid: true},
		{value: "maintainer", want: AuthorAssociation("MAINTAINER")},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			got := NormalizeAuthorAssociation(test.value)
			if got != test.want || got.Valid() != test.valid {
				t.Fatalf("NormalizeAuthorAssociation(%q) = %q, valid %t", test.value, got, got.Valid())
			}
		})
	}
}

func TestNewCandidateResultSortsBeforeApplyingLimit(t *testing.T) {
	t.Parallel()

	earlier := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)
	issues := []Issue{
		{ID: "3", Identifier: "DD-3", CreatedAt: &later},
		{ID: "2", Identifier: "dd-2", CreatedAt: &earlier},
		{ID: "1", Identifier: "DD-1", CreatedAt: &earlier},
	}
	got := NewCandidateResult(issues, CandidateRequest{Limit: 2}, 2, false)
	ids := []string{got.Issues[0].ID, got.Issues[1].ID}
	if !reflect.DeepEqual(ids, []string{"1", "2"}) {
		t.Fatalf("candidate IDs = %#v, want [1 2]", ids)
	}
	if !got.Truncated || got.PagesRead != 2 || got.ItemsRead != 3 {
		t.Fatalf("result = %#v, want three read items, two pages, and truncation", got)
	}
	if issues[0].ID != "3" {
		t.Fatalf("NewCandidateResult mutated input = %#v", issues)
	}
}
