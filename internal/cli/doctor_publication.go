package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

type doctorIssueFilingDestination struct {
	repository    string
	source        string
	allowVerbatim bool
}

func checkDoctorPublicIssueExposure(ctx context.Context, projectID string, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	name := "Project " + projectID + " public issue exposure"
	destinations := doctorIssueFilingDestinations(cfg)
	if len(destinations) == 0 {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "no cross-project issue-filing destinations are configured"}
	}
	deps = deps.withDefaults()
	sourceRepository := strings.TrimSpace(cfg.Tracker.Repository)
	sourceInfo, err := deps.githubRepositoryInfo(ctx, cfg, sourceRepository)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: fmt.Sprintf("source repository visibility for %s could not be determined: %v", sourceRepository, err),
			Hint:   "Restore GitHub repository metadata access; Detent fails closed by redacting connector writes or declining direct Codex issue tools when destination visibility is unknown.",
		}
	}
	if !sourceInfo.Private && !strings.EqualFold(sourceInfo.Visibility, "private") && !strings.EqualFold(sourceInfo.Visibility, "internal") {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "cross-project issue-filing sources are not private"}
	}

	var risks []string
	var unknown []string
	for _, destination := range destinations {
		if destination.allowVerbatim {
			continue
		}
		info, infoErr := deps.githubRepositoryInfo(ctx, cfg, destination.repository)
		if infoErr != nil {
			unknown = append(unknown, destination.repository+" via "+destination.source)
			continue
		}
		if !info.Private && strings.EqualFold(info.Visibility, "public") {
			risks = append(risks, destination.repository+" via "+destination.source)
		}
	}
	sort.Strings(risks)
	sort.Strings(unknown)
	if len(risks) > 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: "private source " + sourceRepository + " files into public destinations: " + strings.Join(risks, ", "),
			Hint:   "Keep Detent publication protection enabled, move issue filing to a private destination, or explicitly opt in only after reviewing the disclosure boundary.",
		}
	}
	if len(unknown) > 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: "destination visibility could not be determined for " + strings.Join(unknown, ", "),
			Hint:   "Restore GitHub repository metadata access; Detent fails closed by redacting connector writes or declining direct Codex issue tools for unknown destinations.",
		}
	}
	return doctorCheck{Name: name, Status: doctorOK, Detail: "private sources do not file into a configured public destination without redaction"}
}

func doctorIssueFilingDestinations(cfg workflowconfig.Config) []doctorIssueFilingDestination {
	sourceRepository := strings.TrimSpace(cfg.Tracker.Repository)
	byKey := map[string]doctorIssueFilingDestination{}
	add := func(repository string, source string, allowVerbatim bool) {
		repository = strings.TrimSpace(repository)
		if repository == "" || strings.EqualFold(repository, sourceRepository) {
			return
		}
		key := strings.ToLower(repository) + "\x00" + source
		byKey[key] = doctorIssueFilingDestination{repository: repository, source: source, allowVerbatim: allowVerbatim}
	}
	if cfg.Retro.Enabled {
		add(cfg.Retro.ProductRepository, "retro.product_repository", cfg.Retro.AllowPublicCrossProjectDetails)
	}
	for _, backend := range cfg.AgentBackendConfigs() {
		if backend.Kind != workflowconfig.AgentBackendCodex {
			continue
		}
		for _, rule := range backend.CodexOptions().DeliverableElicitationAllowlist {
			if !doctorIssueFilingTool(rule.Server, rule.Tool) {
				continue
			}
			add(rule.Repository, "codex.deliverable_elicitation_allowlist", false)
		}
	}
	out := make([]doctorIssueFilingDestination, 0, len(byKey))
	for _, destination := range byKey {
		out = append(out, destination)
	}
	sort.Slice(out, func(i int, j int) bool {
		if out[i].repository == out[j].repository {
			return out[i].source < out[j].source
		}
		return out[i].repository < out[j].repository
	})
	return out
}

func doctorIssueFilingTool(server string, tool string) bool {
	if server != "codex_apps" {
		return false
	}
	switch tool {
	case "github.create_issue", "github.update_issue", "github.add_comment_to_issue", "github.update_issue_comment":
		return true
	default:
		return false
	}
}
