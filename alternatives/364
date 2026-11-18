package doctor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

const (
	WorkflowRuleRunawaySessionTokens           = "runaway_session_tokens"
	workflowRuleReworkLaps                     = "rework_laps"
	workflowRuleValidatorModel                 = "validator_model"
	workflowRuleEmptyModelTelemetry            = "empty_model_telemetry"
	WorkflowRulePinnedRouteModelRejected       = "pinned_route_model_rejected"
	workflowRulePinnedWorkerModelStale         = "pinned_worker_model_stale"
	workflowRuleSessionMultiplierKills         = "session_multiplier_ceiling_kills"
	workflowRuleCompletionEvidenceBelowMedian  = "spend_progress_below_median_session_cost"
	workflowRuleNoSessionTokenBrake            = "no_session_token_brake"
	workflowRuleBudgetEstimateDrift            = "budget_estimate_drift"
	workflowRuleSchedulerSkipRate              = "scheduler_skip_rate"
	workflowRuleInvalidWorkpadStatusRecurrence = "invalid_workpad_status_recurrence"
	workflowRuleOrphanRecoveryFallback         = "orphan_recovery_fallback"
	workflowRuleOrphansNeverRecovered          = "orphans_never_recovered"
	workflowRuleReviewFlowBehaviorMismatch     = "review_flow_behavior_mismatch"
	workflowRuleReviewFlowProseMismatch        = "review_flow_prose_mismatch"
)

type WorkflowOptimizationReport struct {
	Findings  []WorkflowOptimizationFinding `json:"findings,omitempty"`
	Proposals []WorkflowImprovementProposal `json:"proposals,omitempty"`
	Diff      string                        `json:"diff,omitempty"`
	Written   []string                      `json:"written,omitempty"`
}

type WorkflowModelChoice struct {
	Mode   string `json:"mode"`
	Model  string `json:"model,omitempty"`
	Source string `json:"source,omitempty"`
}

type WorkflowOptimizationFinding struct {
	RuleID               string                      `json:"rule_id"`
	ProjectID            string                      `json:"project_id,omitempty"`
	WorkflowPath         string                      `json:"workflow_path,omitempty"`
	Severity             string                      `json:"severity"`
	Title                string                      `json:"title"`
	Detail               string                      `json:"detail"`
	EstimatedTokenImpact int64                       `json:"estimated_token_impact"`
	Evidence             map[string]any              `json:"evidence"`
	Patch                []WorkflowOptimizationPatch `json:"patch,omitempty"`
}

type WorkflowOptimizationPatch struct {
	Path  string `json:"path"`
	Value any    `json:"value"`
}

func (r *WorkflowOptimizationReport) Merge(next WorkflowOptimizationReport) {
	r.Findings = append(r.Findings, next.Findings...)
	r.Proposals = append(r.Proposals, next.Proposals...)
	if r.Diff == "" {
		r.Diff = next.Diff
	} else if strings.TrimSpace(next.Diff) != "" {
		r.Diff = strings.TrimRight(r.Diff, "\n") + "\n" + strings.TrimLeft(next.Diff, "\n")
	}
	r.Written = append(r.Written, next.Written...)
}

func ReviewFlowWorkflowFindings(projectID string, workflowPath string, cfg workflowconfig.Config, prompt string) []WorkflowOptimizationFinding {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil
	}
	choice := ReviewFlowChoice(cfg)
	reviewState := reviewFlowConfiguredState(cfg.Agent.AutoPromote.SourceState, "Human Review")
	passState := reviewFlowConfiguredState(cfg.Agent.AutoPromote.PassState, "Merging")
	switch choice {
	case reviewFlowAutopilot:
		phrases := reviewFlowPhraseMatches(prompt, reviewFlowEnterReviewPhrases(reviewState))
		if phrases == 0 {
			return nil
		}
		return []WorkflowOptimizationFinding{workflowFinding(projectID, workflowPath, workflowRuleReviewFlowProseMismatch,
			"Review-flow prose contradicts autopilot",
			"autopilot is configured but WORKFLOW.md instructs agents to enter the review state",
			0,
			map[string]any{
				"review_flow":           choice,
				"review_state":          reviewState,
				"matching_phrase_count": int64(phrases),
				"auto_promote_enabled":  cfg.Agent.AutoPromote.Enabled,
				"quiet_seconds":         cfg.Agent.AutoPromote.QuietSeconds,
				"gate_wait_state":       ReviewFlowGateWaitState(cfg),
			},
		)}
	case reviewFlowReviewGate:
		phrases := reviewFlowPhraseMatches(prompt, append(
			reviewFlowSkipReviewPhrases(reviewState),
			reviewFlowDirectPromotePhrases(passState)...,
		))
		if phrases == 0 {
			return nil
		}
		return []WorkflowOptimizationFinding{workflowFinding(projectID, workflowPath, workflowRuleReviewFlowProseMismatch,
			"Review-flow prose contradicts review-gate",
			"review-gate is configured but WORKFLOW.md promises direct review-state skipping",
			0,
			map[string]any{
				"review_flow":           choice,
				"review_state":          reviewState,
				"pass_state":            passState,
				"matching_phrase_count": int64(phrases),
				"auto_promote_enabled":  cfg.Agent.AutoPromote.Enabled,
				"quiet_seconds":         cfg.Agent.AutoPromote.QuietSeconds,
				"gate_wait_state":       ReviewFlowGateWaitState(cfg),
			},
		)}
	default:
		return nil
	}
}

func reviewFlowConfiguredState(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func reviewFlowEnterReviewPhrases(reviewState string) []string {
	phrases := []string{}
	for _, state := range reviewFlowStatePhraseVariants(reviewState) {
		phrases = append(phrases,
			"move the issue to "+state,
			"move this issue to "+state,
			"move it to "+state,
			"move the card to "+state,
			"move the work item to "+state,
			"move back to "+state,
		)
	}
	if label := reviewFlowStateLabel(reviewState); label != "" {
		phrases = append(phrases, "detent:"+label)
	}
	return phrases
}

func reviewFlowSkipReviewPhrases(reviewState string) []string {
	phrases := []string{
		"leave the issue in `in progress`",
		"leave the issue in in progress",
	}
	for _, state := range reviewFlowStatePhraseVariants(reviewState) {
		phrases = append(phrases,
			"do not move it to "+state,
			"do not move the issue to "+state,
			"do not move the work item to "+state,
			"never move the issue to "+state,
			"never move the work item to "+state,
			"skips "+state,
			"skip "+state,
		)
	}
	return phrases
}

func reviewFlowDirectPromotePhrases(passState string) []string {
	phrases := []string{}
	for _, state := range reviewFlowStatePhraseVariants(passState) {
		phrases = append(phrases,
			"promotes the issue directly to "+state,
			"promote the issue directly to "+state,
			"promotes the work item directly to "+state,
			"promote the work item directly to "+state,
			"promote directly to "+state,
		)
	}
	return phrases
}

func reviewFlowStatePhraseVariants(state string) []string {
	state = strings.TrimSpace(state)
	if state == "" {
		return nil
	}
	return []string{"`" + state + "`", state}
}

func reviewFlowStateLabel(state string) string {
	state = strings.ToLower(strings.Join(strings.Fields(state), "-"))
	state = strings.TrimSpace(state)
	if state == "" {
		return ""
	}
	var b strings.Builder
	lastHyphen := false
	for _, r := range state {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			lastHyphen = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case r == '-' && !lastHyphen:
			b.WriteRune(r)
			lastHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func reviewFlowPhraseMatches(text string, phrases []string) int {
	count := 0
	for _, phrase := range phrases {
		phrase = strings.ToLower(strings.Join(strings.Fields(phrase), " "))
		if reviewFlowPhraseMatch(text, phrase) {
			count++
		}
	}
	return count
}

func reviewFlowPhraseMatch(text string, phrase string) bool {
	segments := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == ';'
	})
	for _, segment := range segments {
		segment = strings.Join(strings.Fields(segment), " ")
		for offset := 0; offset < len(segment); {
			match := strings.Index(segment[offset:], phrase)
			if match < 0 {
				break
			}
			match += offset
			if !reviewFlowPhraseNegated(segment[:match], segment[match+len(phrase):]) {
				return true
			}
			offset = match + len(phrase)
		}
	}
	return false
}

func reviewFlowPhraseNegated(prefix string, suffix string) bool {
	words := strings.Fields(prefix)
	if len(words) > 5 {
		words = words[len(words)-5:]
	}
	window := " " + strings.Join(words, " ") + " "
	negated := strings.Contains(window, " not ") ||
		strings.Contains(window, " never ") ||
		strings.Contains(window, " don't ") ||
		strings.Contains(window, " avoid ") ||
		strings.Contains(window, " instead of ")
	if !negated {
		return false
	}

	suffix = " " + strings.Join(strings.Fields(suffix), " ") + " "
	return !strings.Contains(suffix, " until ") &&
		!strings.Contains(suffix, " unless ") &&
		!strings.Contains(suffix, " except ")
}

func workflowFinding(
	projectID string,
	workflowPath string,
	ruleID string,
	title string,
	detail string,
	impact int64,
	evidence map[string]any,
	patches ...WorkflowOptimizationPatch,
) WorkflowOptimizationFinding {
	return WorkflowOptimizationFinding{
		RuleID:               ruleID,
		ProjectID:            projectID,
		WorkflowPath:         workflowPath,
		Severity:             "warning",
		Title:                title,
		Detail:               detail,
		EstimatedTokenImpact: impact,
		Evidence:             evidence,
		Patch:                patches,
	}
}

type workflowModelConfig struct {
	Model  string
	Source string
}

func WorkflowWorkerModelChoice(cfg workflowconfig.Config) WorkflowModelChoice {
	modelConfig, ok := workflowDefaultRouteModelConfig(cfg)
	if ok && strings.TrimSpace(modelConfig.Model) != "" {
		return WorkflowModelChoice{
			Mode:   "pinned",
			Model:  strings.TrimSpace(modelConfig.Model),
			Source: modelConfig.Source,
		}
	}
	choice := WorkflowModelChoice{Mode: "provider_default"}
	if ok {
		choice.Source = modelConfig.Source
	}
	return choice
}

func workflowDefaultRouteModelConfig(cfg workflowconfig.Config) (workflowModelConfig, bool) {
	backends := WorkflowBackendConfigsByID(cfg)
	for _, route := range cfg.AgentRouteConfigs() {
		role := strings.ToLower(strings.TrimSpace(route.Role))
		if role != "" && role != "code" {
			continue
		}
		if !route.Default {
			continue
		}
		if model := strings.TrimSpace(route.Model); model != "" {
			return workflowModelConfig{
				Model:  model,
				Source: "agents.routes.model",
			}, true
		}
		backend, ok := backends[strings.TrimSpace(route.Backend)]
		if ok {
			if model := WorkflowBackendCommandModel(backend); model != "" {
				return workflowModelConfig{
					Model:  model,
					Source: "agents.backends.command",
				}, true
			}
		}
		if strings.TrimSpace(route.ModelField) != "" {
			return workflowModelConfig{
				Source: "agents.routes.model_field",
			}, true
		}
	}
	return workflowModelConfig{}, false
}

func WorkflowBackendConfigsByID(cfg workflowconfig.Config) map[string]workflowconfig.AgentBackend {
	backends := cfg.AgentBackendConfigs()
	byID := make(map[string]workflowconfig.AgentBackend, len(backends))
	for _, backend := range backends {
		byID[strings.TrimSpace(backend.ID)] = backend
	}
	return byID
}

func WorkflowBackendCommandModel(backend workflowconfig.AgentBackend) string {
	if strings.TrimSpace(backend.Kind) != workflowconfig.AgentBackendCodex {
		return ""
	}
	return workflowCommandConfigModel(backend.Command)
}

func workflowCommandConfigModel(command string) string {
	fields := WorkflowCommandFields(command)
	for index, field := range fields {
		field = strings.TrimSpace(field)
		if field == "--config" {
			if index+1 >= len(fields) {
				continue
			}
			if model := workflowConfigModel(fields[index+1]); model != "" {
				return model
			}
			continue
		}
		if value, ok := strings.CutPrefix(field, "--config="); ok {
			if model := workflowConfigModel(value); model != "" {
				return model
			}
		}
	}
	return ""
}

func workflowConfigModel(value string) string {
	key, model, ok := strings.Cut(strings.TrimSpace(value), "=")
	if !ok || strings.TrimSpace(key) != "model" {
		return ""
	}
	return strings.Trim(strings.TrimSpace(model), `"'`)
}

func WorkflowCommandFields(command string) []string {
	var fields []string
	var field strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if field.Len() == 0 {
			return
		}
		fields = append(fields, field.String())
		field.Reset()
	}
	for _, r := range command {
		if escaped {
			field.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if quote == 0 && unicode.IsSpace(r) {
			flush()
			continue
		}
		switch {
		case quote == 0 && (r == '\'' || r == '"'):
			quote = r
		case quote != 0 && r == quote:
			quote = 0
		default:
			field.WriteRune(r)
		}
	}
	if escaped {
		field.WriteRune('\\')
	}
	flush()
	return fields
}

func WorkflowOptimizationWorkflowPath(project globalconfig.Project) (string, error) {
	if strings.TrimSpace(project.WorkflowRef) != "" {
		return "", errors.New("workflow_ref-backed workflow cannot be patched locally")
	}
	return ResolveProjectPath(project, project.Workflow)
}

func WriteDoctorWorkflowOptimizationPretty(out io.Writer, report WorkflowOptimizationReport) error {
	if out == nil || len(report.Findings) == 0 && len(report.Proposals) == 0 && strings.TrimSpace(report.Diff) == "" && len(report.Written) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, "Workflow Optimization"); err != nil {
		return err
	}
	for index, finding := range report.Findings {
		if _, err := fmt.Fprintf(out, "%d. [%s] %s", index+1, finding.RuleID, finding.Title); err != nil {
			return err
		}
		if finding.EstimatedTokenImpact > 0 {
			if _, err := fmt.Fprintf(out, " (impact: %d tokens)", finding.EstimatedTokenImpact); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
		if finding.ProjectID != "" {
			if _, err := fmt.Fprintf(out, "   Project: %s\n", finding.ProjectID); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(out, "   Detail: %s\n", finding.Detail); err != nil {
			return err
		}
		if evidence := workflowEvidenceLine(finding.Evidence); evidence != "" {
			if _, err := fmt.Fprintf(out, "   Evidence: %s\n", evidence); err != nil {
				return err
			}
		}
		if patch := workflowPatchLine(finding.Patch); patch != "" {
			if _, err := fmt.Fprintf(out, "   Suggested patch: %s\n", patch); err != nil {
				return err
			}
		}
	}
	if len(report.Proposals) > 0 {
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, "Governed Self-Improvement Proposals"); err != nil {
			return err
		}
		for index, proposal := range report.Proposals {
			if _, err := fmt.Fprintf(out, "%d. [%s] %s\n", index+1, proposal.ID, proposal.Title); err != nil {
				return err
			}
			if proposal.ProjectID != "" {
				if _, err := fmt.Fprintf(out, "   Project: %s\n", proposal.ProjectID); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(out, "   Signal: %s count=%d pattern=%s\n", proposal.SignalKind, proposal.Count, proposal.Pattern); err != nil {
				return err
			}
			target := proposal.TargetKind
			if proposal.TargetPath != "" {
				target += " " + proposal.TargetPath
			}
			if strings.TrimSpace(target) != "" {
				if _, err := fmt.Fprintf(out, "   Target: %s\n", target); err != nil {
					return err
				}
			}
			if proposal.SuggestedChange != "" {
				if _, err := fmt.Fprintf(out, "   Suggested change: %s\n", proposal.SuggestedChange); err != nil {
					return err
				}
			}
			if proposal.Governance != "" {
				if _, err := fmt.Fprintf(out, "   Governance: %s\n", proposal.Governance); err != nil {
					return err
				}
			}
		}
	}
	if diff := strings.TrimSpace(report.Diff); diff != "" {
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, "Proposed WORKFLOW.md Frontmatter Patch"); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, diff); err != nil {
			return err
		}
	}
	if len(report.Written) > 0 {
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "Updated WORKFLOW.md files: %s\n", strings.Join(report.Written, ", ")); err != nil {
			return err
		}
	}
	return nil
}

func workflowEvidenceLine(evidence map[string]any) string {
	if len(evidence) == 0 {
		return ""
	}
	keys := make([]string, 0, len(evidence))
	for key := range evidence {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, evidence[key]))
	}
	return strings.Join(parts, ", ")
}

func workflowPatchLine(patches []WorkflowOptimizationPatch) string {
	if len(patches) == 0 {
		return ""
	}
	parts := make([]string, 0, len(patches))
	for _, patch := range patches {
		parts = append(parts, fmt.Sprintf("%s=%v", patch.Path, patch.Value))
	}
	return strings.Join(parts, ", ")
}

func WriteDoctorWorkflowOptimizationPatches(report WorkflowOptimizationReport) ([]string, error) {
	patches := WorkflowOptimizationPatchesByWorkflow(report)
	if len(patches) == 0 {
		return nil, nil
	}

	paths := make([]string, 0, len(patches))
	for path := range patches {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	written := make([]string, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read workflow %s: %w", path, err)
		}
		updated, err := applyWorkflowOptimizationPatches(raw, patches[path])
		if err != nil {
			return nil, fmt.Errorf("patch workflow %s: %w", path, err)
		}
		workflow, err := workflowconfig.ParseWorkflow(updated)
		if err != nil {
			return nil, fmt.Errorf("validate patched workflow %s: %w", path, err)
		}
		if err := workflow.Config.Validate(); err != nil {
			return nil, fmt.Errorf("validate patched workflow %s: %w", path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat workflow %s: %w", path, err)
		}
		if bytes.Equal(raw, updated) {
			continue
		}
		if err := os.WriteFile(path, updated, info.Mode().Perm()); err != nil {
			return nil, fmt.Errorf("write workflow %s: %w", path, err)
		}
		written = append(written, path)
	}
	return written, nil
}

func WorkflowOptimizationPatchesByWorkflow(report WorkflowOptimizationReport) map[string][]WorkflowOptimizationPatch {
	out := map[string][]WorkflowOptimizationPatch{}
	seen := map[string]map[string]struct{}{}
	for _, finding := range report.Findings {
		workflowPath := strings.TrimSpace(finding.WorkflowPath)
		if workflowPath == "" {
			continue
		}
		if seen[workflowPath] == nil {
			seen[workflowPath] = map[string]struct{}{}
		}
		for _, patch := range finding.Patch {
			patch.Path = strings.TrimSpace(patch.Path)
			if patch.Path == "" {
				continue
			}
			if _, ok := seen[workflowPath][patch.Path]; ok {
				continue
			}
			out[workflowPath] = append(out[workflowPath], patch)
			seen[workflowPath][patch.Path] = struct{}{}
		}
	}
	return out
}

func WorkflowOptimizationDiff(report WorkflowOptimizationReport) string {
	patches := WorkflowOptimizationPatchesByWorkflow(report)
	if len(patches) == 0 {
		return ""
	}
	paths := make([]string, 0, len(patches))
	for path := range patches {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var builder strings.Builder
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		updated, err := applyWorkflowOptimizationPatches(raw, patches[path])
		if err != nil || bytes.Equal(raw, updated) {
			continue
		}
		before, _, err := splitWorkflowFrontmatter(raw)
		if err != nil {
			continue
		}
		after, _, err := splitWorkflowFrontmatter(updated)
		if err != nil {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(frontmatterDiff(path, before, after))
	}
	return strings.TrimRight(builder.String(), "\n")
}

func frontmatterDiff(path string, before []byte, after []byte) string {
	var builder strings.Builder
	builder.WriteString("--- ")
	builder.WriteString(path)
	builder.WriteByte('\n')
	builder.WriteString("+++ ")
	builder.WriteString(path)
	builder.WriteByte('\n')
	builder.WriteString("@@ workflow frontmatter @@\n")
	for _, line := range strings.Split(strings.TrimRight(string(before), "\n"), "\n") {
		if line == "" {
			continue
		}
		builder.WriteByte('-')
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	for _, line := range strings.Split(strings.TrimRight(string(after), "\n"), "\n") {
		if line == "" {
			continue
		}
		builder.WriteByte('+')
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	return builder.String()
}

func applyWorkflowOptimizationPatches(raw []byte, patches []WorkflowOptimizationPatch) ([]byte, error) {
	frontmatter, prompt, err := splitWorkflowFrontmatter(raw)
	if err != nil {
		return nil, err
	}

	root, err := workflowFrontmatterRoot(frontmatter)
	if err != nil {
		return nil, err
	}
	for _, patch := range patches {
		if err := applyWorkflowOptimizationPatch(root, patch); err != nil {
			return nil, err
		}
	}

	frontmatter, err = yaml.Marshal(root)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(bytes.TrimRight(frontmatter, "\n"))
	out.WriteString("\n---\n")
	out.Write(prompt)
	return out.Bytes(), nil
}

func splitWorkflowFrontmatter(raw []byte) ([]byte, []byte, error) {
	normalized := strings.ReplaceAll(strings.TrimPrefix(string(raw), "\ufeff"), "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return nil, nil, errors.New("missing YAML frontmatter")
	}
	body := normalized[len("---\n"):]
	if strings.HasPrefix(body, "---\n") {
		return []byte{}, []byte(body[len("---\n"):]), nil
	}
	if body == "---" {
		return []byte{}, []byte{}, nil
	}
	before, after, ok := strings.Cut(body, "\n---\n")
	if ok {
		return []byte(before), []byte(after), nil
	}
	if before, ok := strings.CutSuffix(body, "\n---"); ok {
		return []byte(before), []byte{}, nil
	}
	return nil, nil, errors.New("unterminated YAML frontmatter")
}

func workflowFrontmatterRoot(frontmatter []byte) (*yaml.Node, error) {
	if len(bytes.TrimSpace(frontmatter)) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(frontmatter, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, errors.New("workflow frontmatter must be a YAML document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("workflow frontmatter must be a mapping")
	}
	return root, nil
}

func applyWorkflowOptimizationPatch(root *yaml.Node, patch WorkflowOptimizationPatch) error {
	path := strings.TrimSpace(patch.Path)
	switch path {
	case "agent.max_session_tokens", "agent.max_session_context_multiplier", "agent.auto_promote.rework_limit", "gate.validator.model", "polling.interval_ms", "budget.per_issue_max_usd":
		return setSimpleYAMLPath(root, strings.Split(patch.Path, "."), patch.Value)
	case "agents.routes.default.model":
		return setDefaultRouteModel(root, fmt.Sprint(patch.Value))
	default:
		if index, ok := workflowRouteModelPatchIndex(path); ok {
			return setRouteModel(root, index, fmt.Sprint(patch.Value))
		}
		return fmt.Errorf("unsupported workflow optimization patch path %q", patch.Path)
	}
}

func setSimpleYAMLPath(root *yaml.Node, path []string, value any) error {
	if len(path) == 0 {
		return errors.New("empty YAML path")
	}
	current := root
	for _, key := range path[:len(path)-1] {
		current = ensureYAMLMapping(current, key)
	}
	setYAMLMappingValue(current, path[len(path)-1], yAMLScalar(value))
	return nil
}

func setDefaultRouteModel(root *yaml.Node, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("default route model is required")
	}
	agents := ensureYAMLMapping(root, "agents")
	routes := yAMLMappingValue(agents, "routes")
	if routes == nil || routes.Kind != yaml.SequenceNode {
		routes = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setYAMLMappingValue(agents, "routes", routes)
	}
	for _, route := range routes.Content {
		if route.Kind != yaml.MappingNode {
			continue
		}
		if yAMLRouteIsDefault(route) {
			setYAMLMappingValue(route, "model", yAMLScalar(model))
			return nil
		}
	}
	routes.Content = append(routes.Content, yAMLMapping(map[string]any{
		"name":    "default",
		"backend": workflowconfig.DefaultAgentBackendID,
		"default": true,
		"model":   model,
	}))
	return nil
}

func workflowRouteModelPatchIndex(path string) (int, bool) {
	path = strings.TrimSpace(path)
	const prefix = "agents.routes["
	const suffix = "].model"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return 0, false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	index, err := strconv.Atoi(value)
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func setRouteModel(root *yaml.Node, index int, model string) error {
	agents := yAMLMappingValue(root, "agents")
	if agents == nil || agents.Kind != yaml.MappingNode {
		return errors.New("agents routes are not configured")
	}
	routes := yAMLMappingValue(agents, "routes")
	if routes == nil || routes.Kind != yaml.SequenceNode {
		return errors.New("agents.routes are not configured")
	}
	if index < 0 || index >= len(routes.Content) {
		return fmt.Errorf("agents.routes[%d] does not exist", index)
	}
	route := routes.Content[index]
	if route.Kind != yaml.MappingNode {
		return fmt.Errorf("agents.routes[%d] is not a mapping", index)
	}
	setYAMLMappingValue(route, "model", yAMLScalar(strings.TrimSpace(model)))
	return nil
}

func yAMLRouteIsDefault(route *yaml.Node) bool {
	defaultValue := yAMLMappingValue(route, "default")
	if defaultValue == nil || defaultValue.Kind != yaml.ScalarNode || defaultValue.Value != "true" {
		return false
	}
	role := yAMLMappingValue(route, "role")
	return role == nil || strings.TrimSpace(role.Value) == "" || strings.EqualFold(strings.TrimSpace(role.Value), "code")
}

func ensureYAMLMapping(parent *yaml.Node, key string) *yaml.Node {
	value := yAMLMappingValue(parent, key)
	if value != nil && value.Kind == yaml.MappingNode {
		return value
	}
	value = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setYAMLMappingValue(parent, key, value)
	return value
}

func yAMLMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func setYAMLMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, yAMLScalar(key), value)
}

func yAMLMapping(values map[string]any) *yaml.Node {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, key := range keys {
		node.Content = append(node.Content, yAMLScalar(key), yAMLScalar(values[key]))
	}
	return node
}

func yAMLScalar(value any) *yaml.Node {
	node := &yaml.Node{Kind: yaml.ScalarNode}
	switch typed := value.(type) {
	case bool:
		node.Tag = "!!bool"
		node.Value = strconv.FormatBool(typed)
	case int:
		node.Tag = "!!int"
		node.Value = strconv.Itoa(typed)
	case int64:
		node.Tag = "!!int"
		node.Value = strconv.FormatInt(typed, 10)
	case float64:
		node.Tag = "!!float"
		node.Value = strconv.FormatFloat(typed, 'f', -1, 64)
	case string:
		node.Tag = "!!str"
		node.Value = typed
	default:
		node.Tag = "!!str"
		node.Value = fmt.Sprint(typed)
	}
	return node
}
