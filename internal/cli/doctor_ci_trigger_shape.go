package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func defaultDoctorWorkflowSources(ctx context.Context, cfg workflowconfig.Config, repository string) (_ map[string]string, resultErr error) {
	conn, err := ghconnector.NewConnector(doctorGitHubConnectorConfig(cfg))
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, conn.Close()) }()
	return conn.FetchWorkflowSources(ctx, repository)
}

func checkDoctorCITriggerShape(ctx context.Context, id string, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	check := doctorCheck{Name: "Project " + id + " ci_trigger_shape", Status: doctorOK}
	if !doctorTrackerUsesGitHubReads(cfg.Tracker.Kind) {
		check.Detail = "not a GitHub tracker; not applicable"
		return check
	}
	if len(cfg.Gate.RequiredStatusChecks) == 0 {
		check.Detail = "no required status checks configured"
		return check
	}
	read := deps.githubWorkflows
	if read == nil {
		check.Status = doctorWarn
		check.Detail = "workflow evidence reader unavailable"
		return check
	}
	sources, err := read(ctx, cfg, cfg.Tracker.Repository)
	if err != nil {
		check.Status = doctorWarn
		check.Detail = "workflow evidence unavailable: " + err.Error()
		return check
	}
	var paths []string
	for path := range sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	matched := map[string]bool{}
	var problems []string
	for _, path := range paths {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(sources[path]), &doc); err != nil {
			problems = append(problems, path+": "+err.Error())
			continue
		}
		if len(doc.Content) == 0 {
			problems = append(problems, path+": empty workflow")
			continue
		}
		root := doc.Content[0]
		jobs := doctorYAMLMapValue(root, "jobs")
		if jobs == nil || jobs.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(jobs.Content); i += 2 {
			jobID, job := jobs.Content[i].Value, jobs.Content[i+1]
			for _, required := range cfg.Gate.RequiredStatusChecks {
				if !doctorRequiredCheckMatchesJob(required, jobID, doctorYAMLScalarValue(doctorYAMLMapValue(job, "name"))) {
					continue
				}
				matched[required] = true
				if !doctorWorkflowHasLabeled(doctorYAMLMapValue(root, "on")) {
					continue
				}
				concurrency := doctorYAMLMapValue(job, "concurrency")
				if concurrency == nil {
					concurrency = doctorYAMLMapValue(root, "concurrency")
				}
				if !doctorHeadSHAConcurrency(concurrency) {
					problems = append(problems, fmt.Sprintf("%s job %s (%s) triggers on labeled without head-SHA concurrency", path, jobID, required))
				}
			}
		}
	}
	for _, required := range cfg.Gate.RequiredStatusChecks {
		if !matched[required] {
			problems = append(problems, "required check "+required+" has no matching workflow job")
		}
	}
	if len(problems) > 0 {
		check.Status = doctorWarn
		check.Detail = strings.Join(problems, "; ")
	} else {
		check.Detail = "required-check workflows have no labeled trigger without head-SHA concurrency"
	}
	check.Hint = "Use a concurrency group containing ${{ github.event.pull_request.head.sha }}; workflow files are read from the remote default branch."
	return check
}

func doctorWorkflowHasLabeled(on *yaml.Node) bool {
	if on == nil {
		return false
	}
	if on.Kind == yaml.ScalarNode {
		return false
	}
	if on.Kind == yaml.SequenceNode {
		return slices.ContainsFunc(on.Content, doctorWorkflowHasLabeled)
	}
	for _, event := range []string{"pull_request", "pull_request_target"} {
		spec := doctorYAMLMapValue(on, event)
		if spec == nil {
			continue
		}
		types := doctorYAMLMapValue(spec, "types")
		// GitHub's default pull_request activities do not include labeled.
		if types == nil {
			continue
		}
		for _, activity := range types.Content {
			if activity.Value == "labeled" {
				return true
			}
		}
	}
	return false
}

func doctorHeadSHAConcurrency(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.MappingNode {
		node = doctorYAMLMapValue(node, "group")
	}
	group := doctorYAMLScalarValue(node)
	return strings.Contains(group, "${{") && strings.Contains(group, "github.event.pull_request.head.sha") && strings.Contains(group, "}}")
}
