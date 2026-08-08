package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
)

type doctorCapabilityState string

const (
	doctorCapabilityConfigured  doctorCapabilityState = "configured"
	doctorCapabilityUnused      doctorCapabilityState = "unused"
	doctorCapabilityUnavailable doctorCapabilityState = "unavailable"
)

type doctorCapability struct {
	Name   string                `json:"name"`
	State  doctorCapabilityState `json:"state"`
	Detail string                `json:"detail"`
}

type doctorCapabilityReport struct {
	ProjectID    string             `json:"project_id"`
	Capabilities []doctorCapability `json:"items"`
}

func checkDoctorCapabilities(project globalconfig.Project, workflow workflowconfig.Workflow) doctorCheck {
	projectID := doctorProjectID(project)
	capabilities := doctorProjectCapabilities(project, workflow)
	counts := map[doctorCapabilityState]int{}
	for _, capability := range capabilities {
		counts[capability.State]++
	}
	return doctorCheck{
		Name:   "Project " + projectID + " capabilities",
		Status: doctorOK,
		Detail: fmt.Sprintf(
			"advisory: %d configured, %d unused, %d unavailable",
			counts[doctorCapabilityConfigured],
			counts[doctorCapabilityUnused],
			counts[doctorCapabilityUnavailable],
		),
		Capabilities: &doctorCapabilityReport{
			ProjectID:    projectID,
			Capabilities: capabilities,
		},
	}
}

func doctorProjectCapabilities(project globalconfig.Project, workflow workflowconfig.Workflow) []doctorCapability {
	cfg := workflow.Config
	intakeEnabled := cfg.Intake.Enabled()
	intakeCount := len(cfg.Intake.Sources)
	if project.IntakeConfigured {
		intakeEnabled = project.Intake.Enabled()
		intakeCount = len(project.Intake.Sources)
	}

	return []doctorCapability{
		doctorBacklogAdmissionCapability(cfg, workflow.SharedPrompt),
		doctorUntrackedAdmissionCapability(cfg),
		doctorExcludeLabelsAdmissionCapability(cfg),
		doctorRoutineCapability(cfg),
		doctorIntakeCapability(cfg, intakeEnabled, intakeCount),
		doctorEnabledCapability(
			"agent.followups",
			cfg.Agent.Followups.Enabled,
			"enabled; agents receive guidance to file separate backlog issues for meaningful out-of-scope discoveries.",
			"Enable to prompt agents to file separate backlog issues for meaningful out-of-scope discoveries.",
		),
		doctorEnabledCapability(
			"agent.auto_promote",
			cfg.Agent.AutoPromote.Enabled,
			"enabled; review-ready work advances automatically after configured gates and quiet windows.",
			"Enable to advance review-ready work automatically after configured gates and quiet windows.",
		),
		doctorRetroCapability(cfg),
		doctorReleaseCapability(cfg),
		doctorEffortRubricCapability(project, cfg),
	}
}

func doctorBacklogAdmissionCapability(cfg workflowconfig.Config, sharedPrompt string) doctorCapability {
	if cfg.BacklogAdmission.Enabled {
		return doctorCapability{
			Name:   "backlog_admission",
			State:  doctorCapabilityConfigured,
			Detail: "enabled; Detent evaluates configured backlog sources against WORKFLOW.md admission criteria.",
		}
	}

	candidateCapabilities := connector.CandidateCapabilitiesFor(connector.Backend(cfg.Tracker.Kind), cfg.Tracker.GitHubStatusSource)
	if len(candidateCapabilities.Selectors) == 0 {
		return doctorCapability{
			Name:   "backlog_admission",
			State:  doctorCapabilityUnavailable,
			Detail: "tracker.kind " + cfg.Tracker.Kind + " does not provide candidate selectors required for backlog admission.",
		}
	}
	if _, err := workflowconfig.ResolveAdmissionCriteria(sharedPrompt, cfg.BacklogAdmission.CriteriaSection); err != nil {
		return doctorCapability{
			Name:   "backlog_admission",
			State:  doctorCapabilityUnavailable,
			Detail: "Prerequisite: set backlog_admission.criteria_section to a shared WORKFLOW.md heading with at least one admission dimension; enabling then evaluates backlog candidates and proposes or admits matching work. Current prerequisite: " + err.Error() + ".",
		}
	}
	return doctorCapability{
		Name:   "backlog_admission",
		State:  doctorCapabilityUnused,
		Detail: "Enable to evaluate backlog candidates against WORKFLOW.md criteria and propose or admit matching work.",
	}
}

func doctorUntrackedAdmissionCapability(cfg workflowconfig.Config) doctorCapability {
	capabilities := connector.CandidateCapabilitiesFor(connector.Backend(cfg.Tracker.Kind), cfg.Tracker.GitHubStatusSource)
	if !capabilities.Supports(connector.CandidateSelectorUntracked) {
		reason := "tracker.kind " + cfg.Tracker.Kind + " does not provide GitHub label-status drift needed to identify untracked issues."
		if cfg.Tracker.Kind == workflowconfig.TrackerGitHub && cfg.Tracker.GitHubStatusSource == workflowconfig.GitHubStatusSourceProjectV2 {
			reason = "github project_v2 status cannot identify untracked repository issues; use github label status to enable this selector."
		}
		return doctorCapability{Name: "backlog_admission.sources.untracked", State: doctorCapabilityUnavailable, Detail: reason}
	}
	return doctorEnabledCapability(
		"backlog_admission.sources.untracked",
		cfg.BacklogAdmission.Sources.Untracked,
		"configured; admission can consider repository issues outside Detent's tracked status lanes.",
		"Enable to admit open repository issues not represented in Detent's label-status lanes.",
	)
}

func doctorExcludeLabelsAdmissionCapability(cfg workflowconfig.Config) doctorCapability {
	if cfg.Tracker.Kind == workflowconfig.TrackerGitHub && cfg.Tracker.GitHubStatusSource == workflowconfig.GitHubStatusSourceProjectV2 {
		return doctorCapability{
			Name:   "backlog_admission.exclude_labels",
			State:  doctorCapabilityUnavailable,
			Detail: "github project_v2 fetches only the first 20 issue labels, so Detent cannot apply exclude_labels completely.",
		}
	}
	return doctorEnabledCapability(
		"backlog_admission.exclude_labels",
		len(cfg.BacklogAdmission.ExcludeLabels) > 0,
		fmt.Sprintf("configured with %d label exclusion(s); matching issues stay out of admission candidates.", len(cfg.BacklogAdmission.ExcludeLabels)),
		"Configure to keep issues with selected labels out of backlog admission candidates.",
	)
}

func doctorRoutineCapability(cfg workflowconfig.Config) doctorCapability {
	if len(cfg.Routines) > 0 {
		return doctorCapability{
			Name:   "routines",
			State:  doctorCapabilityConfigured,
			Detail: fmt.Sprintf("%d scheduled routine(s) run recurring agent prompts.", len(cfg.Routines)),
		}
	}
	if cfg.Tracker.Kind != workflowconfig.TrackerGitHub && cfg.Tracker.Kind != workflowconfig.TrackerMemory {
		return doctorCapability{
			Name:   "routines",
			State:  doctorCapabilityUnavailable,
			Detail: "tracker.kind " + cfg.Tracker.Kind + " does not support scheduled routine issue creation; routines require github or memory.",
		}
	}
	return doctorCapability{Name: "routines", State: doctorCapabilityUnused, Detail: "Enable to run recurring agent prompts on a schedule."}
}

func doctorIntakeCapability(cfg workflowconfig.Config, enabled bool, count int) doctorCapability {
	if enabled {
		return doctorCapability{
			Name:   "intake.sources",
			State:  doctorCapabilityConfigured,
			Detail: fmt.Sprintf("%d source(s) turn external or scheduled signals into tracker issues.", count),
		}
	}
	if cfg.Tracker.Kind != workflowconfig.TrackerGitHub {
		return doctorCapability{
			Name:   "intake.sources",
			State:  doctorCapabilityUnavailable,
			Detail: "tracker.kind " + cfg.Tracker.Kind + " cannot create intake issues; intake.sources requires github.",
		}
	}
	return doctorCapability{Name: "intake.sources", State: doctorCapabilityUnused, Detail: "Enable to turn external webhook or scheduled signals into tracker issues."}
}

func doctorRetroCapability(cfg workflowconfig.Config) doctorCapability {
	if cfg.Retro.Enabled {
		return doctorCapability{
			Name:   "retro",
			State:  doctorCapabilityConfigured,
			Detail: "enabled; recurring failure telemetry can become governed improvement issues.",
		}
	}
	if cfg.Tracker.Kind != workflowconfig.TrackerGitHub && cfg.Tracker.Kind != workflowconfig.TrackerMemory {
		return doctorCapability{
			Name:   "retro",
			State:  doctorCapabilityUnavailable,
			Detail: "tracker.kind " + cfg.Tracker.Kind + " cannot create retrospective issues; retro requires github or memory.",
		}
	}
	return doctorCapability{Name: "retro", State: doctorCapabilityUnused, Detail: "Enable to turn recurring failure telemetry into governed improvement issues."}
}

func doctorReleaseCapability(cfg workflowconfig.Config) doctorCapability {
	if cfg.Release.Enabled {
		return doctorCapability{
			Name:   "release",
			State:  doctorCapabilityConfigured,
			Detail: "enabled; Detent cuts releases after configured merge-count, age, and CI gates.",
		}
	}
	if cfg.Tracker.Kind != workflowconfig.TrackerGitHub && cfg.Tracker.Kind != workflowconfig.TrackerGitHubLocal {
		return doctorCapability{
			Name:   "release",
			State:  doctorCapabilityUnavailable,
			Detail: "tracker.kind " + cfg.Tracker.Kind + " cannot inspect and tag GitHub repositories; release requires github or github_local.",
		}
	}
	return doctorCapability{Name: "release", State: doctorCapabilityUnused, Detail: "Enable to cut releases automatically after merge-count, age, and CI gates."}
}

func doctorEffortRubricCapability(project globalconfig.Project, cfg workflowconfig.Config) doctorCapability {
	root := projectSourceRoot(project, cfg)
	if root == "" {
		return doctorCapability{Name: "detent-agent effort rubric", State: doctorCapabilityUnavailable, Detail: "source root is not configured, so AGENTS.md and CLAUDE.md cannot be inspected."}
	}
	expandedRoot, err := expandDoctorWorkspacePath(root)
	if err != nil {
		return doctorCapability{Name: "detent-agent effort rubric", State: doctorCapabilityUnavailable, Detail: "source root could not be resolved: " + err.Error() + "."}
	}
	info, err := os.Stat(expandedRoot)
	if err != nil || !info.IsDir() {
		return doctorCapability{Name: "detent-agent effort rubric", State: doctorCapabilityUnavailable, Detail: "source repository is unavailable locally, so AGENTS.md and CLAUDE.md cannot be inspected."}
	}
	path, err := findDoctorIssueEffortGuidance(expandedRoot)
	if err != nil {
		return doctorCapability{Name: "detent-agent effort rubric", State: doctorCapabilityUnavailable, Detail: err.Error() + "."}
	}
	if path != "" {
		return doctorCapability{Name: "detent-agent effort rubric", State: doctorCapabilityConfigured, Detail: path + " provides project-specific detent-agent effort guidance."}
	}
	return doctorCapability{Name: "detent-agent effort rubric", State: doctorCapabilityUnused, Detail: "Add a project-specific detent-agent effort rubric so issues receive deliberate reasoning effort."}
}

func doctorEnabledCapability(name string, enabled bool, configuredDetail string, unusedDetail string) doctorCapability {
	if enabled {
		return doctorCapability{Name: name, State: doctorCapabilityConfigured, Detail: configuredDetail}
	}
	return doctorCapability{Name: name, State: doctorCapabilityUnused, Detail: unusedDetail}
}

func findDoctorIssueEffortGuidance(sourceRoot string) (_ string, resultErr error) {
	root, err := os.OpenRoot(sourceRoot)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("source root could not be opened: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, root.Close())
	}()

	for _, path := range []string{"AGENTS.md", "CLAUDE.md"} {
		content, err := root.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("%s could not be read: %w", path, err)
		}
		if strings.Contains(strings.ToLower(string(content)), "detent-agent") {
			return path, nil
		}
	}
	return "", nil
}
