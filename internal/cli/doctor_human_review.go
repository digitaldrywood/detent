package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func checkDoctorHumanReviewLane(ctx context.Context, id string, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	name := "Project " + id + " Human Review policy"
	if cfg.Review.Human {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "review.human=true"}
	}
	if cfg.Tracker.Kind == workflowconfig.TrackerHubNative {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: "review.human=false; native Human Review occupancy could not be inspected"}
	}
	if deps.autoPromoteConnector == nil {
		deps.autoPromoteConnector = defaultDoctorAutoPromoteConnector
	}
	projectConnector, err := deps.autoPromoteConnector(cfg)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("Human Review occupancy unavailable: %v", err)}
	}
	if projectConnector == nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: "Human Review occupancy unavailable: connector is nil"}
	}
	defer closeDoctorAutoPromoteConnector(projectConnector)
	states := doctorHumanReviewStates(cfg)
	scan, err := fetchDoctorAutoPromoteIssues(ctx, projectConnector, states)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("read Human Review occupancy: %v", err)}
	}
	count := 0
	for _, state := range states {
		count += doctorStateScanCount(scan.EnumeratedCounts, state)
	}
	if count == 0 {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "review.human=false; no issues in Human Review"}
	}
	labels := make([]string, 0, len(scan.Issues))
	for _, issue := range scan.Issues {
		labels = append(labels, doctorIssueLabel(issue))
	}
	return doctorCheck{
		Name: name, Status: doctorFail,
		Detail: fmt.Sprintf("review.human=false but %d issue(s) remain in Human Review: %s", count, strings.Join(labels, ", ")),
		Hint:   "Move these issues to Blocked or another appropriate lane; review the recorded reason before resuming work.",
	}
}

func checkDoctorNativeHumanReviewLane(ctx context.Context, cfg globalconfig.Config, project globalconfig.Project, deps doctorDeps) (doctorCheck, bool) {
	projectID := cfg.Client.NativeProjects[project.ID]
	if projectID == "" {
		return doctorCheck{}, false
	}
	workflow, err := loadDoctorProjectWorkflow(ctx, project, deps)
	if err != nil || workflow.Config.Review.Human {
		return doctorCheck{}, false
	}
	name := "Project " + project.ID + " Human Review policy"
	warn := func(err error) (doctorCheck, bool) {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: "native Human Review occupancy unavailable: " + err.Error()}, true
	}
	lookup := deps.lookupEnv
	if lookup == nil {
		lookup = os.Getenv
	}
	clientConfig := cfg.Client.Normalized()
	client, err := hubclient.New(hubclient.Config{URL: clientConfig.URL, IdentityFile: clientConfig.IdentityFile, TokenSource: func() string { return lookup(clientConfig.TokenEnvironment) }})
	if err != nil {
		return warn(err)
	}
	native, err := client.Native(tracker.OrganizationID(cfg.Client.OrganizationID), tracker.ProjectID(projectID))
	if err != nil {
		return warn(err)
	}
	for _, state := range doctorHumanReviewStates(workflow.Config) {
		page, err := native.Issues(ctx, url.Values{"state": {state}, "limit": {"1"}})
		if err != nil {
			return warn(err)
		}
		if len(page.Items) == 0 {
			continue
		}
		return doctorCheck{
			Name: name, Status: doctorFail,
			Detail: fmt.Sprintf("review.human=false but issue #%d is in %s", page.Items[0].Number, state),
			Hint:   "Move the issue to Blocked or another appropriate lane; review its recorded reason before resuming work.",
		}, true
	}
	return doctorCheck{Name: name, Status: doctorOK, Detail: "review.human=false; no issues in Human Review"}, true
}

func doctorHumanReviewStates(cfg workflowconfig.Config) []string {
	states := []string{"Human Review"}
	state := strings.TrimSpace(cfg.Agent.AutoPromote.SourceState)
	if state != "" && !strings.EqualFold(state, states[0]) {
		states = append(states, state)
	}
	return states
}
