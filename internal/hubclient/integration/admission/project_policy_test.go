package admission_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
)

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

type policyAdmissionSource struct {
	orchestrator.SchedulingSource
	failure error
}

func (s policyAdmissionSource) ConnectorForProject(string) (connector.Connector, bool) {
	return fakehub.New(fakehub.Config{}), true
}

func (s policyAdmissionSource) CheckProjectPolicy(context.Context, string, string, policy.Descriptor) error {
	return s.failure
}

func TestProjectPolicyAdmissionPreservesConflict(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"first rejection", "still pending"} {
		t.Run(name, func(t *testing.T) {
			workflow, err := workflowconfig.ParseWorkflow([]byte("---\ntracker:\n  kind: hub_native\n---\nKeep human review holds.\n"))
			if err != nil {
				t.Fatal(err)
			}
			source := policyAdmissionSource{failure: &hubclient.APIError{Status: http.StatusConflict, Code: "policy_mismatch", Message: "repository policy approval pending"}}
			loaded, err := project.New(project.Config{Project: globalconfig.Project{ID: "example", Workdir: t.TempDir()}, Workflow: workflow}, project.Dependencies{Scheduling: source, Runner: orchestrator.FakeRunner{}})
			if loaded != nil {
				t.Cleanup(func() {
					if err := loaded.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			var failure *hubclient.APIError
			if !errors.As(err, &failure) || failure.Status != http.StatusConflict || failure.Code != "policy_mismatch" {
				t.Fatalf("pending approval lost its API conflict: %v", err)
			}
		})
	}
}
