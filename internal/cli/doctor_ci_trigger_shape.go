package cli

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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
	var policyProblems []string
	if deps.githubBranchPolicy != nil && strings.TrimSpace(cfg.Tracker.Repository) != "" {
		policy, err := deps.githubBranchPolicy(ctx, cfg, cfg.Tracker.Repository)
		if err != nil {
			policyProblems = append(policyProblems, "branch policy evidence unavailable: "+err.Error())
		} else {
			if policy.RulesUnavailableOnPlan {
				policyProblems = append(policyProblems, "branch rules unavailable on the repository plan")
			}
			cfg.Gate.RequiredStatusChecks = uniqueDoctorStrings(append(append([]string(nil), cfg.Gate.RequiredStatusChecks...), policy.RequiredStatusChecks...))
		}
	}
	if len(cfg.Gate.RequiredStatusChecks) == 0 {
		check.Detail = "no required status checks configured or discovered"
		if len(policyProblems) > 0 {
			check.Status = doctorWarn
			check.Detail = strings.Join(policyProblems, "; ")
		}
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
	problems := policyProblems
	producers := map[string][]doctorCIProducer{}
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
				if !doctorRequiredCheckMatchesJob(required, jobID, doctorYAMLScalarValue(doctorYAMLMapValue(job, "name"))) && !doctorJobPostsContext(job, required) {
					continue
				}
				matched[required] = true
				producers[required] = append(producers[required], doctorCIProducersForContext(root, job, jobID, required)...)
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
	problems = append(problems, doctorCITriggerGaps(cfg.Gate.RequiredStatusChecks, cfg.Gate.CITriggerLabel, producers)...)
	if len(problems) > 0 {
		check.Status = doctorWarn
		check.Detail = strings.Join(problems, "; ")
	} else {
		check.Detail = "required-check workflows have no labeled trigger without head-SHA concurrency"
	}
	check.Hint = "Configure gate.ci_trigger_label for label-gated required checks or enable automatic PR CI. Use a concurrency group containing ${{ github.event.pull_request.head.sha }}; workflow files are read from the remote default branch."
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

// Static workflow analysis only claims label support for literal, recognizable
// conditions. Arbitrary scripts and expressions remain unverified evidence.
type doctorCIProducer struct {
	automatic     bool
	labeled       bool
	label         string
	unconditional bool
}

var doctorCILabelEquality = regexp.MustCompile(`^\s*(?:\$\{\{\s*)?github\.event\.label\.name\s*==\s*['"]([^'"]+)['"]\s*(?:\}\})?\s*$`)
var doctorCILabelContains = regexp.MustCompile(`^\s*(?:\$\{\{\s*)?contains\(\s*github\.event\.pull_request\.labels\.\*\.name\s*,\s*['"]([^'"]+)['"]\s*\)\s*(?:\}\})?\s*$`)

func doctorCIProducerFor(root, job *yaml.Node) doctorCIProducer {
	on := doctorYAMLMapValue(root, "on")
	condition := strings.TrimSpace(doctorYAMLScalarValue(doctorYAMLMapValue(job, "if")))
	p := doctorCIProducer{labeled: doctorWorkflowHasLabeled(on), unconditional: condition == "" || condition == "true" || condition == "${{ true }}" || condition == "always()" || condition == "${{ always() }}"}
	for _, pattern := range []*regexp.Regexp{doctorCILabelEquality, doctorCILabelContains} {
		if match := pattern.FindStringSubmatch(condition); len(match) > 1 {
			p.label = match[1]
		}
	}
	p.automatic = p.unconditional && doctorWorkflowAutomaticPR(on)
	return p
}

func doctorWorkflowAutomaticPR(on *yaml.Node) bool {
	if on == nil {
		return false
	}
	if on.Kind == yaml.ScalarNode {
		return on.Value == "pull_request" || on.Value == "pull_request_target" || on.Value == "push"
	}
	if on.Kind == yaml.SequenceNode {
		return slices.ContainsFunc(on.Content, doctorWorkflowAutomaticPR)
	}
	for _, event := range []string{"pull_request", "pull_request_target", "push"} {
		spec := doctorYAMLMapValue(on, event)
		if spec == nil {
			continue
		}
		// Filtered workflows cannot guarantee a status for every PR.
		filtered := false
		for _, key := range []string{"branches", "branches-ignore", "paths", "paths-ignore", "tags", "tags-ignore"} {
			if doctorYAMLMapValue(spec, key) != nil {
				filtered = true
			}
		}
		if filtered {
			continue
		}
		types := doctorYAMLMapValue(spec, "types")
		if types == nil {
			return true
		}
		opened, synchronize := false, false
		for _, activity := range types.Content {
			opened = opened || activity.Value == "opened"
			synchronize = synchronize || activity.Value == "synchronize"
		}
		if opened && synchronize {
			return true
		}
	}
	return false
}

func doctorCITriggerGaps(required []string, configured string, producers map[string][]doctorCIProducer) []string {
	var problems []string
	for _, context := range required {
		automatic, triggered := false, false
		var labels []string
		for _, p := range producers[context] {
			automatic = automatic || p.automatic
			if p.labeled {
				if p.label != "" {
					labels = append(labels, p.label)
				}
				triggered = triggered || (configured != "" && (p.unconditional || p.label == configured))
			}
		}
		if automatic || triggered {
			continue
		}
		labels = uniqueDoctorStrings(labels)
		sort.Strings(labels)
		detail := "required status " + context + " has no verified automatic PR producer; "
		if len(labels) > 0 {
			detail += "configure gate.ci_trigger_label: " + strings.Join(labels, " or gate.ci_trigger_label: ")
		} else {
			detail += "Detent cannot trigger a verified producer; enable pull_request opened/synchronize CI or configure the status producer"
		}
		problems = append(problems, detail)
	}
	return problems
}

func doctorJobPostsContext(job *yaml.Node, required string) bool {
	// GitHub-script and shell status publishers commonly use literal context
	// fields. A match identifies a candidate, not proof that arbitrary code runs.
	steps := doctorYAMLMapValue(job, "steps")
	if steps == nil {
		return false
	}
	return doctorNodePostsContext(steps, required)
}

func doctorNodePostsContext(node *yaml.Node, required string) bool {
	raw, err := yaml.Marshal(node)
	if err != nil {
		return false
	}
	pattern := regexp.MustCompile(`(?:['"]?context['"]?\s*:\s*['"]|context=['"]?)([^'"\n]+)`)
	for _, match := range pattern.FindAllStringSubmatch(string(raw), -1) {
		if strings.TrimSpace(match[1]) == required {
			return true
		}
	}
	return false
}

func doctorCIProducersForContext(root, job *yaml.Node, jobID, required string) []doctorCIProducer {
	if doctorRequiredCheckMatchesJob(required, jobID, doctorYAMLScalarValue(doctorYAMLMapValue(job, "name"))) {
		return []doctorCIProducer{doctorCIProducerFor(root, job)}
	}
	var result []doctorCIProducer
	steps := doctorYAMLMapValue(job, "steps")
	if steps == nil {
		return nil
	}
	for _, step := range steps.Content {
		if !doctorNodePostsContext(step, required) {
			continue
		}
		producer := doctorCIProducerFor(root, job)
		condition := strings.TrimSpace(doctorYAMLScalarValue(doctorYAMLMapValue(step, "if")))
		if producer.unconditional && condition != "" {
			producer = doctorCIProducerFor(root, step)
		} else if condition != "" && condition != "always()" && condition != "${{ always() }}" {
			producer.automatic = false
			producer.label = ""
			producer.unconditional = false
		}
		result = append(result, producer)
	}
	return result
}
