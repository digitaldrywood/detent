package cli

import (
	"context"
	"fmt"
	"os"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func checkDoctorHubPolicy(ctx context.Context, cfg globalconfig.Config, selected globalconfig.Project, deps doctorDeps) doctorCheck {
	deps = deps.withDefaults()
	for _, configured := range project.ManagerConfigFromGlobal(cfg).Projects {
		if configured.ID == selected.ID {
			selected = configured
			break
		}
	}
	check := doctorCheck{Name: "Project " + selected.ID + " repository policy", Status: doctorFail, Hint: "Run detent hub policy inspect --config " + cfg.Path + " --project " + selected.ID + "; load the approved definition or ask an administrator to approve the reviewed descriptor."}
	workflow, err := loadDoctorProjectWorkflow(ctx, selected, deps)
	if err != nil {
		check.Detail = err.Error()
		return check
	}
	descriptor, err := project.ResolvePolicy(selected, workflow)
	if err != nil {
		check.Detail = err.Error()
		return check
	}
	lookup := deps.lookupEnv
	if lookup == nil {
		lookup = os.Getenv
	}
	clientConfig := cfg.Client.Normalized()
	client, err := hubclient.New(hubclient.Config{URL: clientConfig.URL, IdentityFile: clientConfig.IdentityFile, TokenSource: func() string { return lookup(clientConfig.TokenEnvironment) }})
	if err != nil {
		check.Detail = err.Error()
		return check
	}
	var approval policy.Approval
	if id := cfg.Client.NativeProjects[selected.ID]; id != "" {
		native, nativeErr := client.Native(tracker.OrganizationID(cfg.Client.OrganizationID), tracker.ProjectID(id))
		if nativeErr != nil {
			check.Detail = nativeErr.Error()
			return check
		}
		approval, err = native.ProjectPolicy(ctx)
	} else {
		approval, err = client.ProjectPolicy(ctx, workflow.Config.Tracker.Repository)
	}
	if err == nil {
		err = descriptor.Match(approval.Policy)
	}
	if err != nil {
		check.Detail = err.Error()
		return check
	}
	check.Status, check.Hint = doctorOK, ""
	check.Detail = fmt.Sprintf("policy %s; source revision %s; approved by %s at %s; gate=%s, auto_promote=%t, merge_method=%s", descriptor.ID, descriptor.SourceRevision, approval.ApprovedBy, approval.ApprovedAt, descriptor.Gates.Kind, descriptor.Gates.AutoPromote, descriptor.Gates.MergeMethod)
	return check
}
