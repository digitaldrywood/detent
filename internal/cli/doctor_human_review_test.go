package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
)

func TestDoctorHumanReviewLane(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		human  bool
		issues []connector.Issue
		want   doctorStatus
	}{
		{name: "empty opt out", want: doctorOK},
		{name: "stranded opt out", issues: []connector.Issue{{ID: "1", Identifier: "owner/repo#1", State: "Human Review"}}, want: doctorFail},
		{name: "review enabled", human: true, want: doctorOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := workflowconfig.Default()
			cfg.Review.Human = tt.human
			cfg.Tracker.Kind = workflowconfig.TrackerMemory
			deps := doctorDeps{autoPromoteConnector: func(workflowconfig.Config) (doctorAutoPromoteConnector, error) {
				return &fakeDoctorAutoPromoteConnector{issues: tt.issues}, nil
			}}
			got := checkDoctorHumanReviewLane(t.Context(), "project", cfg, deps)
			if got.Status != tt.want {
				t.Fatalf("check = %#v, want %s", got, tt.want)
			}
			if tt.want == doctorFail && !strings.Contains(got.Detail, "owner/repo#1") {
				t.Fatalf("check detail = %q, want issue identity", got.Detail)
			}
		})
	}
}

func TestDoctorNativeHumanReviewLane(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, response string
		want           doctorStatus
	}{
		{name: "empty", response: `{"items":[]}`, want: doctorOK},
		{name: "stranded", response: `{"items":[{"number":42,"state":"Human Review"}]}`, want: doctorFail},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/work-items") || r.URL.Query().Get("state") != "Human Review" {
					t.Errorf("unexpected request: %s", r.URL.String())
				}
				_, _ = w.Write([]byte(tt.response))
			}))
			t.Cleanup(server.Close)
			cfg := globalconfig.Config{}
			cfg.Client.URL = server.URL
			cfg.Client.OrganizationID = "org_example"
			cfg.Client.NativeProjects = map[string]string{"project": "prj_example"}
			deps := doctorDeps{
				lookupEnv: func(string) string { return "test-token" },
				loadWorkflow: func(string) (workflowconfig.Workflow, error) {
					return workflowconfig.Workflow{Config: workflowconfig.Default()}, nil
				},
			}
			check, ok := checkDoctorNativeHumanReviewLane(t.Context(), cfg, globalconfig.Project{ID: "project", Workflow: "WORKFLOW.md"}, deps)
			if !ok || check.Status != tt.want {
				t.Fatalf("check = %#v, ok = %t, want %s", check, ok, tt.want)
			}
		})
	}
}
