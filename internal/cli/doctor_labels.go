package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

type doctorConfiguredLabel struct {
	ConfigKey   string
	Name        string
	Description string
	Color       string
	Critical    bool
}

func defaultDoctorGitHubRepositoryLabels(
	ctx context.Context,
	cfg workflowconfig.Config,
	repository string,
) (_ []string, resultErr error) {
	conn, err := ghconnector.NewConnector(doctorGitHubConnectorConfig(cfg))
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, conn.Close())
	}()
	return conn.FetchRepositoryLabels(ctx, repository)
}

func checkDoctorConfiguredLabels(
	ctx context.Context,
	projectID string,
	project globalconfig.Project,
	cfg workflowconfig.Config,
	deps doctorDeps,
) doctorCheck {
	name := "Project " + projectID + " configured labels"
	required := doctorConfiguredLabels(cfg)
	if len(required) == 0 {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "no escape-hatch labels are configured"}
	}
	if deps.githubLabels == nil {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: "configured label verification skipped because the GitHub label reader is unavailable",
			Hint:   "Rerun detent doctor with GitHub repository access.",
		}
	}

	repositories := doctorGitHubRepositories(ctx, project, cfg, deps, projectSourceRoot(project, cfg))
	if len(repositories) == 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: "configured label verification skipped because no GitHub repository could be resolved",
			Hint:   "Set tracker.repository to owner/repo in detent.yaml.",
		}
	}
	sort.Strings(repositories)

	missing := make([]string, 0)
	fixes := make([]string, 0)
	seenFixes := map[string]struct{}{}
	status := doctorOK
	for _, repository := range repositories {
		existing, err := deps.githubLabels(ctx, cfg, repository)
		if err != nil {
			return doctorCheck{
				Name:   name,
				Status: doctorFail,
				Detail: fmt.Sprintf("read %s labels: %v", repository, err),
				Hint:   "Fix GitHub repository label read access, then rerun detent doctor.",
			}
		}
		existingSet := doctorNormalizedLabelSet(existing)
		for _, label := range required {
			if _, ok := existingSet[strings.ToLower(label.Name)]; ok {
				continue
			}
			missing = append(missing, fmt.Sprintf("%s missing label %q referenced by %s", repository, label.Name, label.ConfigKey))
			if label.Critical {
				status = doctorFail
			} else if status == doctorOK {
				status = doctorWarn
			}
			fix := doctorConfiguredLabelFix(repository, label)
			if _, ok := seenFixes[fix]; !ok {
				seenFixes[fix] = struct{}{}
				fixes = append(fixes, fix)
			}
		}
	}
	if len(missing) == 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: fmt.Sprintf("verified %d configured labels in %s", len(required)*len(repositories), strings.Join(repositories, ", ")),
		}
	}
	return doctorCheck{
		Name:   name,
		Status: status,
		Detail: strings.Join(missing, "; "),
		Hint:   strings.Join(fixes, "; "),
	}
}

func doctorConfiguredLabels(cfg workflowconfig.Config) []doctorConfiguredLabel {
	labels := make([]doctorConfiguredLabel, 0, 2)
	if name := strings.TrimSpace(cfg.Agent.AutoPromote.OptoutLabel); name != "" {
		labels = append(labels, doctorConfiguredLabel{
			ConfigKey:   "agent.auto_promote.optout_label",
			Name:        name,
			Description: "Excludes an issue from Detent auto-promotion.",
			Color:       "b60205",
			Critical:    true,
		})
	}
	if name := strings.TrimSpace(cfg.Agent.MaxSessionTokenOverrideLabel); name != "" {
		labels = append(labels, doctorConfiguredLabel{
			ConfigKey:   "agent.max_session_token_override_label",
			Name:        name,
			Description: "Allows an issue to exceed the configured agent session token ceiling.",
			Color:       "fbca04",
		})
	}
	return labels
}

func doctorNormalizedLabelSet(labels []string) map[string]struct{} {
	set := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		if normalized := strings.ToLower(strings.TrimSpace(label)); normalized != "" {
			set[normalized] = struct{}{}
		}
	}
	return set
}

func doctorConfiguredLabelFix(repository string, label doctorConfiguredLabel) string {
	return "gh label create " + doctorShellQuote(label.Name) +
		" --repo " + doctorShellQuote(repository) +
		" --color " + label.Color +
		" --description " + doctorShellQuote(label.Description)
}
