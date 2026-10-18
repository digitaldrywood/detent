package onboarding

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/workload"
)

const (
	DeliveryProfileFullAutopilot      = "full_autopilot"
	DeliveryProfileReviewGate         = "review_gate"
	DeliveryProfileConservativeManual = "conservative_manual"
	DeliveryProfileAutonomousDelivery = DeliveryProfileFullAutopilot
	DeliveryProfileConservativeReview = DeliveryProfileConservativeManual
	IntakeProfileManual               = "manual_intake"
	IntakeProfileAssisted             = "assisted_intake"
	IntakeProfileAutonomous           = "autonomous_intake"
	IntakeProfileCriteriaSection      = "Admission Criteria"
	IntakeProfileRoutineName          = "repository-maintenance"
	IntakeProfileRoutineSchedule      = "0 6 * * 1"
	IntakeProfileRoutinePrompt        = "Review the repository against the Admission Criteria in WORKFLOW.md. File only scoped findings with repository evidence and explicit acceptance criteria. Each issue body must include a fenced `detent-agent` block with `schema: 1` and a best-guess `effort` selected from the project rubric."
	IntakeProfileStaleTODOsSchedule   = "0 6 * * 1"
	IntakeProfileTrustedAssociations  = "OWNER,MEMBER,COLLABORATOR"
)

type DeliveryProfileSettings struct {
	ID                           string
	Label                        string
	KanbanMode                   string
	AutoPromoteEnabled           bool
	AutoPromoteQuietSeconds      int
	AutoPromoteGateWaitState     string
	GateRequireAutomatedReview   bool
	DependencyAutoUnblockEnabled bool
	MergingConcurrency           int
}

type DeliveryProfileSummary struct {
	EffectiveDeliveryProfile      string           `json:"effective_delivery_profile"`
	EffectiveDeliveryProfileLabel string           `json:"effective_delivery_profile_label"`
	KanbanMode                    string           `json:"kanban_mode"`
	KanbanBehavior                string           `json:"kanban_behavior"`
	GateRequiresAutomatedReview   bool             `json:"gate_requires_automated_review"`
	GateBehavior                  string           `json:"gate_behavior"`
	AutoPromoteEnabled            bool             `json:"auto_promote_enabled"`
	AutoPromoteQuietSeconds       int              `json:"auto_promote_quiet_seconds"`
	AutoPromotionBehavior         string           `json:"auto_promotion_behavior"`
	QuietWindowBehavior           string           `json:"quiet_window_behavior"`
	DependencyAutoUnblockEnabled  bool             `json:"dependency_auto_unblock_enabled"`
	DependencyAutoUnblockBehavior string           `json:"dependency_auto_unblock_behavior"`
	MergingConcurrency            int              `json:"merging_concurrency"`
	MergeConcurrencyBehavior      string           `json:"merge_concurrency_behavior"`
	StopBehavior                  string           `json:"stop_behavior"`
	StopConditions                []string         `json:"stop_conditions"`
	AgentPoolChoice               *AgentPoolChoice `json:"agent_pool_choice,omitempty"`
}

type IntakeProfileSettings struct {
	ID                            string
	Label                         string
	FollowupsEnabled              bool
	BacklogAdmissionEnabled       bool
	BacklogAdmissionAutoAdmit     bool
	BacklogAdmissionMinConfidence float64
	RoutinesEnabled               bool
	StaleTODOsEnabled             bool
}

type IntakeProfileSummary struct {
	EffectiveIntakeProfile      string `json:"effective_intake_profile"`
	EffectiveIntakeProfileLabel string `json:"effective_intake_profile_label"`
	FollowupsEnabled            bool   `json:"followups_enabled"`
	BacklogAdmissionEnabled     bool   `json:"backlog_admission_enabled"`
	BacklogAdmissionAutoAdmit   bool   `json:"backlog_admission_auto_admit"`
	RoutinesEnabled             bool   `json:"routines_enabled"`
	StaleTODOsEnabled           bool   `json:"stale_todos_enabled"`
	Behavior                    string `json:"behavior"`
}

type ProjectWorkload struct {
	ID       string
	Workflow workflowconfig.Config
}

type AgentPoolChoice struct {
	Question      string            `json:"question"`
	Options       []AgentPoolOption `json:"options"`
	LocalProjects []string          `json:"local_projects"`
	CloudProjects []string          `json:"cloud_projects"`
}

type AgentPoolOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

func NormalizeDeliveryProfile(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "full", "full-autopilot", "full_autopilot", "autopilot", "maximum", "max", "autonomous", "autonomous-delivery", "autonomous_delivery":
		return DeliveryProfileFullAutopilot
	case "review-gate", "review_gate":
		return DeliveryProfileReviewGate
	case "conservative", "conservative-manual", "conservative_manual", "manual", "review", "human_review", "human-review", "conservative-review", "conservative_review":
		return DeliveryProfileConservativeManual
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func DeliveryProfile(value string) (DeliveryProfileSettings, bool) {
	switch NormalizeDeliveryProfile(value) {
	case DeliveryProfileFullAutopilot:
		return DeliveryProfileSettings{
			ID:                           DeliveryProfileFullAutopilot,
			Label:                        "Full autopilot",
			KanbanMode:                   "integration",
			AutoPromoteEnabled:           true,
			AutoPromoteQuietSeconds:      0,
			AutoPromoteGateWaitState:     "source",
			GateRequireAutomatedReview:   false,
			DependencyAutoUnblockEnabled: true,
			MergingConcurrency:           1,
		}, true
	case DeliveryProfileReviewGate:
		return DeliveryProfileSettings{
			ID:                           DeliveryProfileReviewGate,
			Label:                        "Review gate",
			KanbanMode:                   "integration",
			AutoPromoteEnabled:           false,
			AutoPromoteQuietSeconds:      600,
			AutoPromoteGateWaitState:     "review",
			GateRequireAutomatedReview:   false,
			DependencyAutoUnblockEnabled: false,
			MergingConcurrency:           1,
		}, true
	case DeliveryProfileConservativeManual:
		return DeliveryProfileSettings{
			ID:                           DeliveryProfileConservativeManual,
			Label:                        "Conservative/manual",
			KanbanMode:                   "read_only",
			AutoPromoteEnabled:           false,
			AutoPromoteQuietSeconds:      600,
			AutoPromoteGateWaitState:     "review",
			GateRequireAutomatedReview:   true,
			DependencyAutoUnblockEnabled: false,
			MergingConcurrency:           1,
		}, true
	default:
		return DeliveryProfileSettings{}, false
	}
}

func SummarizeDeliveryProfile(value string) (DeliveryProfileSummary, bool) {
	settings, ok := DeliveryProfile(value)
	if !ok {
		return DeliveryProfileSummary{}, false
	}
	return DeliveryProfileSummary{
		EffectiveDeliveryProfile:      settings.ID,
		EffectiveDeliveryProfileLabel: settings.Label,
		KanbanMode:                    settings.KanbanMode,
		KanbanBehavior:                kanbanBehavior(settings),
		GateRequiresAutomatedReview:   settings.GateRequireAutomatedReview,
		GateBehavior:                  gateBehavior(settings),
		AutoPromoteEnabled:            settings.AutoPromoteEnabled,
		AutoPromoteQuietSeconds:       settings.AutoPromoteQuietSeconds,
		AutoPromotionBehavior:         autoPromotionBehavior(settings),
		QuietWindowBehavior:           quietWindowBehavior(settings),
		DependencyAutoUnblockEnabled:  settings.DependencyAutoUnblockEnabled,
		DependencyAutoUnblockBehavior: dependencyAutoUnblockBehavior(settings),
		MergingConcurrency:            settings.MergingConcurrency,
		MergeConcurrencyBehavior:      mergeConcurrencyBehavior(settings),
		StopBehavior:                  "Existing validation, CI, unresolved review feedback, dependency blockers, mergeability, and gate failures still stop progress.",
		StopConditions: []string{
			"validation failures",
			"CI failures",
			"unresolved review feedback",
			"dependency blockers",
			"mergeability problems",
			"gate failures",
		},
	}, true
}

func SummarizeDeliveryProfileForProjects(
	value string,
	projects []ProjectWorkload,
) (DeliveryProfileSummary, bool) {
	summary, ok := SummarizeDeliveryProfile(value)
	if !ok {
		return DeliveryProfileSummary{}, false
	}
	if choice, offered := AgentPoolChoiceForProjects(projects); offered {
		summary.AgentPoolChoice = &choice
	}
	return summary, true
}

func AgentPoolChoiceForProjects(projects []ProjectWorkload) (AgentPoolChoice, bool) {
	if len(projects) < 2 {
		return AgentPoolChoice{}, false
	}
	var choice AgentPoolChoice
	for _, project := range projects {
		id := strings.TrimSpace(project.ID)
		if id == "" {
			continue
		}
		class, _ := workload.Classify(project.Workflow)
		switch class {
		case workload.ClassLocalHeavy:
			choice.LocalProjects = append(choice.LocalProjects, id)
		case workload.ClassCloudOnly:
			choice.CloudProjects = append(choice.CloudProjects, id)
		}
	}
	if len(choice.LocalProjects) == 0 || len(choice.CloudProjects) == 0 {
		return AgentPoolChoice{}, false
	}
	sort.Strings(choice.LocalProjects)
	sort.Strings(choice.CloudProjects)
	choice.Question = "Optional: keep local validation/build work and cloud-only model work in separate agent pools?"
	choice.Options = []AgentPoolOption{
		{
			ID:          "split",
			Label:       "Use separate pools",
			Description: "Start with code and cloud pools so each workload class can be tuned independently.",
		},
		{
			ID:          "shared",
			Label:       "Keep one pool",
			Description: "Continue with one shared capacity setting and decide on partitioning later.",
		},
	}
	return choice, true
}

func DeliveryProfileAnswerExpansion(value string) (map[string]string, bool) {
	settings, ok := DeliveryProfile(value)
	if !ok {
		return nil, false
	}
	return map[string]string{
		"KANBAN_MODE":                           settings.KanbanMode,
		"AUTO_PROMOTE_ENABLED":                  strconv.FormatBool(settings.AutoPromoteEnabled),
		"AUTO_PROMOTE_QUIET_SECONDS":            strconv.Itoa(settings.AutoPromoteQuietSeconds),
		"AUTO_PROMOTE_GATE_WAIT_STATE":          settings.AutoPromoteGateWaitState,
		"GATE_REQUIRE_AUTOMATED_REVIEW":         strconv.FormatBool(settings.GateRequireAutomatedReview),
		"AUTO_PROMOTE_REQUIRE_AUTOMATED_REVIEW": strconv.FormatBool(settings.GateRequireAutomatedReview),
		"DEPENDENCY_AUTO_UNBLOCK_ENABLED":       strconv.FormatBool(settings.DependencyAutoUnblockEnabled),
		"MERGING_CONCURRENCY":                   strconv.Itoa(settings.MergingConcurrency),
	}, true
}

func NormalizeIntakeProfile(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "manual", "manual-intake", "manual_intake":
		return IntakeProfileManual
	case "assisted", "assisted-intake", "assisted_intake":
		return IntakeProfileAssisted
	case "autonomous", "autonomous-intake", "autonomous_intake":
		return IntakeProfileAutonomous
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func IntakeProfile(value string) (IntakeProfileSettings, bool) {
	switch NormalizeIntakeProfile(value) {
	case IntakeProfileManual:
		return IntakeProfileSettings{
			ID:               IntakeProfileManual,
			Label:            "Manual intake",
			FollowupsEnabled: true,
		}, true
	case IntakeProfileAssisted:
		return IntakeProfileSettings{
			ID:                            IntakeProfileAssisted,
			Label:                         "Assisted intake",
			FollowupsEnabled:              true,
			BacklogAdmissionEnabled:       true,
			BacklogAdmissionMinConfidence: workflowconfig.DefaultBacklogAdmissionAutoAdmitMinConfidence,
		}, true
	case IntakeProfileAutonomous:
		return IntakeProfileSettings{
			ID:                            IntakeProfileAutonomous,
			Label:                         "Autonomous intake",
			FollowupsEnabled:              true,
			BacklogAdmissionEnabled:       true,
			BacklogAdmissionAutoAdmit:     true,
			BacklogAdmissionMinConfidence: workflowconfig.DefaultBacklogAdmissionAutoAdmitMinConfidence,
			RoutinesEnabled:               true,
			StaleTODOsEnabled:             true,
		}, true
	default:
		return IntakeProfileSettings{}, false
	}
}

func IntakeProfileAnswerExpansion(value string) (map[string]string, bool) {
	settings, ok := IntakeProfile(value)
	if !ok {
		return nil, false
	}
	answers := map[string]string{
		"FOLLOWUPS_ENABLED":         strconv.FormatBool(settings.FollowupsEnabled),
		"BACKLOG_ADMISSION_ENABLED": strconv.FormatBool(settings.BacklogAdmissionEnabled),
		"ROUTINES_ENABLED":          strconv.FormatBool(settings.RoutinesEnabled),
		"STALE_TODOS_ENABLED":       strconv.FormatBool(settings.StaleTODOsEnabled),
	}
	if settings.BacklogAdmissionEnabled {
		answers["BACKLOG_ADMISSION_SCHEDULE"] = workflowconfig.DefaultBacklogAdmissionSchedule
		answers["BACKLOG_ADMISSION_SOURCE_STATE"] = "Backlog"
		answers["BACKLOG_ADMISSION_TARGET_STATE"] = "Todo"
		answers["BACKLOG_ADMISSION_CRITERIA_SECTION"] = IntakeProfileCriteriaSection
		answers["BACKLOG_ADMISSION_MAX_CANDIDATES_PER_RUN"] = strconv.Itoa(workflowconfig.DefaultBacklogAdmissionMaxCandidatesPerRun)
		answers["BACKLOG_ADMISSION_MAX_PROPOSALS_PER_RUN"] = strconv.Itoa(workflowconfig.DefaultBacklogAdmissionMaxProposalsPerRun)
		answers["BACKLOG_ADMISSION_MAX_OPEN_PROPOSALS"] = strconv.Itoa(workflowconfig.DefaultBacklogAdmissionMaxOpenProposals)
		answers["BACKLOG_ADMISSION_PROPOSAL_EXPIRY_DAYS"] = strconv.Itoa(workflowconfig.DefaultBacklogAdmissionProposalExpiryDays)
		answers["BACKLOG_ADMISSION_AUTO_ADMIT"] = strconv.FormatBool(settings.BacklogAdmissionAutoAdmit)
		answers["BACKLOG_ADMISSION_AUTO_ADMIT_MIN_CONFIDENCE"] = strconv.FormatFloat(settings.BacklogAdmissionMinConfidence, 'f', -1, 64)
		if settings.BacklogAdmissionAutoAdmit {
			answers["BACKLOG_ADMISSION_AUTHORS_ALLOW_ASSOCIATION"] = IntakeProfileTrustedAssociations
		}
	}
	if settings.RoutinesEnabled {
		answers["ROUTINE_NAME"] = IntakeProfileRoutineName
		answers["ROUTINE_SCHEDULE"] = IntakeProfileRoutineSchedule
		answers["ROUTINE_PROMPT"] = IntakeProfileRoutinePrompt
	}
	if settings.StaleTODOsEnabled {
		answers["STALE_TODOS_SCHEDULE"] = IntakeProfileStaleTODOsSchedule
	}
	return answers, true
}

func SummarizeIntakeProfile(value string) (IntakeProfileSummary, bool) {
	settings, ok := IntakeProfile(value)
	if !ok {
		return IntakeProfileSummary{}, false
	}
	behavior := "Humans and implementer follow-ups place work in Backlog; Detent does not promote or discover additional work automatically."
	if settings.BacklogAdmissionEnabled {
		behavior = "Detent evaluates Backlog work and proposes promotion to Todo for operator approval."
	}
	if settings.BacklogAdmissionAutoAdmit {
		behavior = "Detent scans for stale TODOs, runs a scheduled repository-maintenance sweep, and automatically admits qualifying Backlog work at or above the configured confidence floor."
	}
	return IntakeProfileSummary{
		EffectiveIntakeProfile:      settings.ID,
		EffectiveIntakeProfileLabel: settings.Label,
		FollowupsEnabled:            settings.FollowupsEnabled,
		BacklogAdmissionEnabled:     settings.BacklogAdmissionEnabled,
		BacklogAdmissionAutoAdmit:   settings.BacklogAdmissionAutoAdmit,
		RoutinesEnabled:             settings.RoutinesEnabled,
		StaleTODOsEnabled:           settings.StaleTODOsEnabled,
		Behavior:                    behavior,
	}, true
}

func SortedIntakeProfileAnswerKeys(answers map[string]string) []string {
	keys := make([]string, 0, len(answers))
	for key := range answers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func SortedDeliveryProfileAnswerKeys(answers map[string]string) []string {
	keys := make([]string, 0, len(answers))
	for key := range answers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func kanbanBehavior(settings DeliveryProfileSettings) string {
	switch settings.KanbanMode {
	case "integration":
		return "Detent can move issues through the configured workflow states instead of only observing them."
	case "read_only":
		return "Detent reads workflow status without mutating Kanban state."
	default:
		return fmt.Sprintf("Detent uses Kanban mode %q.", settings.KanbanMode)
	}
}

func gateBehavior(settings DeliveryProfileSettings) string {
	if settings.GateRequireAutomatedReview {
		return "Automated GitHub PR review is required before the command gate and promotion checks can pass."
	}
	return "No automated GitHub PR review is required when the command gate is passing and the workflow says so."
}

func autoPromotionBehavior(settings DeliveryProfileSettings) string {
	if settings.AutoPromoteEnabled && settings.AutoPromoteQuietSeconds == 0 && settings.AutoPromoteGateWaitState == "source" {
		return "Detent keeps completed work in the active state and promotes it to `Merging` when the linked PR, local gate, CI, and guardrails pass."
	}
	if settings.AutoPromoteEnabled {
		return "Detent automatically promotes eligible work from `Human Review` to `Merging` when the linked PR, local gate, CI, and guardrails pass."
	}
	return "Detent stops in `Human Review` until an operator approves promotion to `Merging`."
}

func quietWindowBehavior(settings DeliveryProfileSettings) string {
	if settings.AutoPromoteEnabled && settings.AutoPromoteQuietSeconds == 0 {
		return "There is no quiet-window delay before promotion."
	}
	if settings.AutoPromoteEnabled {
		return fmt.Sprintf("Detent waits %d seconds of quiet time before promotion after readiness checks pass.", settings.AutoPromoteQuietSeconds)
	}
	return fmt.Sprintf("Auto-promotion is disabled; the %d-second quiet window only matters if auto-promotion is enabled later.", settings.AutoPromoteQuietSeconds)
}

func dependencyAutoUnblockBehavior(settings DeliveryProfileSettings) string {
	if settings.DependencyAutoUnblockEnabled {
		return "Dependency-waiting `Blocked` issues can move back to `Todo` when declared blockers are terminal or merged."
	}
	return "Dependency-waiting `Blocked` issues remain `Blocked` until a human or workflow moves them."
}

func mergeConcurrencyBehavior(settings DeliveryProfileSettings) string {
	if settings.MergingConcurrency == 1 {
		return "`Merging` remains serialized for this project."
	}
	return fmt.Sprintf("Up to %d issues can be in `Merging` concurrently for this project.", settings.MergingConcurrency)
}
