package cli

import (
	"context"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
)

func TestDoctorOwnershipEligibility(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mode       string
		required   bool
		assignees  []string
		labels     []string
		wantStatus doctorStatus
		wantCount  int
		wantDetail string
	}{
		{name: "assigned steady state", mode: workflowconfig.IdentityOwnershipAssignee, required: true, assignees: []string{"operator"}, labels: []string{"detent"}, wantStatus: doctorOK, wantDetail: "all label-eligible active issues have an assignee"},
		{name: "upgrade grace preflight", mode: workflowconfig.IdentityOwnershipAssignee, labels: []string{"detent"}, wantStatus: doctorWarn, wantCount: 1, wantDetail: "would stop dispatching"},
		{name: "acknowledged rule blocks", mode: workflowconfig.IdentityOwnershipAssignee, required: true, labels: []string{"detent"}, wantStatus: doctorWarn, wantCount: 1, wantDetail: "cannot dispatch"},
		{name: "other ownership mode", mode: workflowconfig.IdentityOwnershipField, labels: []string{"detent"}, wantStatus: doctorOK},
		{name: "authorization label absent", mode: workflowconfig.IdentityOwnershipAssignee, wantStatus: doctorOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := workflowconfig.Default()
			cfg.Identity = workflowconfig.Identity{Name: "operator", OwnershipMode: tt.mode, AssigneeRequired: tt.required, OwnerField: "Owner"}
			if tt.mode == workflowconfig.IdentityOwnershipAssignee {
				cfg.Identity.OwnerField = ""
			}
			cfg.Tracker.ActiveStates = []string{"Todo"}
			cfg.Tracker.Authorization = selector.Selector{Labels: selector.Labels{Include: []string{"detent"}}}
			issue := connector.Issue{ID: "issue-473", Identifier: "gopherguides/corp#473", State: "Todo", Labels: tt.labels, Assignees: tt.assignees}
			check := checkDoctorOwnershipEligibility(context.Background(), "corp", globalconfig.Project{}, cfg, doctorDeps{
				autoPromoteConnector: func(workflowconfig.Config) (doctorAutoPromoteConnector, error) {
					return &fakeDoctorAutoPromoteConnector{issues: []connector.Issue{issue}}, nil
				},
			})

			if check.Status != tt.wantStatus || len(check.OwnershipAttention) != tt.wantCount {
				t.Fatalf("check = %#v, want status %s and %d diagnostics", check, tt.wantStatus, tt.wantCount)
			}
			if tt.wantDetail != "" && !strings.Contains(check.Detail, tt.wantDetail) {
				t.Fatalf("check = %#v, want detail containing %q", check, tt.wantDetail)
			}
			if tt.wantCount > 0 && check.OwnershipAttention[0].Remedy != "assign the issue" {
				t.Fatalf("check = %#v, want assignee remedy", check)
			}
		})
	}
}

func TestDoctorAuthorizationEligibilityReportsLaneLabeledMismatch(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	cfg.Tracker.StatusLabelPrefix = "detent:"
	cfg.Tracker.ActiveStates = []string{"Todo"}
	cfg.Tracker.ObservedStates = []string{"Blocked"}
	cfg.Tracker.TerminalStates = []string{"Done"}
	issues := []connector.Issue{
		{ID: "issue-532", Identifier: "gopherguides/corp#532", State: "Todo", Labels: []string{"detent:todo"}},
		{ID: "issue-authorized", Identifier: "gopherguides/corp#533", State: "Todo", Labels: []string{"detent:todo", "detent"}},
		{ID: "issue-untracked", Identifier: "gopherguides/corp#534", State: "Todo"},
		{ID: "issue-closed", Identifier: "gopherguides/corp#535", State: "Done", Closed: true, Labels: []string{"detent:done"}},
	}
	tracker := &doctorAuthorizationConnector{issues: issues}
	check := checkDoctorAuthorizationEligibility(t.Context(), "corp", globalconfig.Project{
		Authorization: selector.Selector{Labels: selector.Labels{Include: []string{"detent"}}},
	}, cfg, doctorDeps{
		autoPromoteConnector: func(workflowconfig.Config) (doctorAutoPromoteConnector, error) {
			return tracker, nil
		},
	})

	if check.Status != doctorWarn || len(check.AuthorizationAttention) != 1 {
		t.Fatalf("check = %#v, want one authorization warning", check)
	}
	diagnostic := check.AuthorizationAttention[0]
	if diagnostic.IssueIdentifier != "gopherguides/corp#532" || diagnostic.Rule != selector.RuleLabelInclude || diagnostic.Value != "detent" {
		t.Fatalf("authorization diagnostic = %#v", diagnostic)
	}
	for _, want := range []string{"gopherguides/corp#532", "missing required label `detent`"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("check detail = %q, want containing %q", check.Detail, want)
		}
	}
	wantStates := []string{"Blocked", "Done", "Todo"}
	if len(tracker.states) != len(wantStates) {
		t.Fatalf("states = %#v, want %#v", tracker.states, wantStates)
	}
	for index := range wantStates {
		if tracker.states[index] != wantStates[index] {
			t.Fatalf("states = %#v, want %#v", tracker.states, wantStates)
		}
	}
}

func TestDoctorAuthorizationEligibilityDistinguishesConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		project    globalconfig.Project
		wantStatus doctorStatus
		wantDetail string
	}{
		{name: "empty selector allows all", wantStatus: doctorOK, wantDetail: "authorization selector is empty; all issues are allowed"},
		{
			name: "malformed selector fails",
			project: globalconfig.Project{Authorization: selector.Selector{
				Labels: selector.Labels{Include: []string{""}},
			}},
			wantStatus: doctorFail,
			wantDetail: "authorization selector is invalid: global.authorization.labels.include[0] must not be blank",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			check := checkDoctorAuthorizationEligibility(t.Context(), "corp", tt.project, workflowconfig.Default(), doctorDeps{})
			if check.Status != tt.wantStatus || check.Detail != tt.wantDetail {
				t.Fatalf("check = %#v, want status %s detail %q", check, tt.wantStatus, tt.wantDetail)
			}
		})
	}
}

type doctorAuthorizationConnector struct {
	issues []connector.Issue
	states []string
}

func (c *doctorAuthorizationConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	c.states = append([]string(nil), states...)
	return append([]connector.Issue(nil), c.issues...), nil
}
