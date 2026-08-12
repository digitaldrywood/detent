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
		assignees  []string
		labels     []string
		wantStatus doctorStatus
		wantCount  int
	}{
		{name: "assignee present", mode: workflowconfig.IdentityOwnershipAssignee, assignees: []string{"operator"}, labels: []string{"detent"}, wantStatus: doctorOK},
		{name: "assignee absent", mode: workflowconfig.IdentityOwnershipAssignee, labels: []string{"detent"}, wantStatus: doctorWarn, wantCount: 1},
		{name: "other ownership mode", mode: workflowconfig.IdentityOwnershipField, labels: []string{"detent"}, wantStatus: doctorOK},
		{name: "authorization label absent", mode: workflowconfig.IdentityOwnershipAssignee, wantStatus: doctorOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := workflowconfig.Default()
			cfg.Identity = workflowconfig.Identity{Name: "operator", OwnershipMode: tt.mode, OwnerField: "Owner"}
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
			if tt.wantCount > 0 && (!strings.Contains(check.Detail, "cannot dispatch") || check.OwnershipAttention[0].Remedy != "assign the issue") {
				t.Fatalf("check = %#v, want dispatch failure with assignee remedy", check)
			}
		})
	}
}
