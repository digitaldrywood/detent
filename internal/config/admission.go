package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/robfig/cron/v3"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	// DefaultBacklogAdmissionSchedule keeps proposal latency under 15 minutes.
	// Observed admission runs cost about 25 seconds and 36,000 tokens each.
	DefaultBacklogAdmissionSchedule               = "*/15 * * * *"
	DefaultBacklogAdmissionMaxCandidatesPerRun    = 50
	DefaultBacklogAdmissionMaxProposalsPerRun     = 3
	DefaultBacklogAdmissionMaxOpenProposals       = 10
	DefaultBacklogAdmissionProposalExpiryDays     = 7
	DefaultBacklogAdmissionAutoAdmitMinConfidence = 0.9
	BacklogAdmissionEffortFileWorkflow            = "WORKFLOW.md"
	BacklogAdmissionEffortFileAgents              = "AGENTS.md"
)

var (
	admissionDimensionPattern = regexp.MustCompile(`^\s*[-*+]\s+\*\*([^*]+)\*\*\s*(?:[—–:-]\s*)?(.+?)\s*$`)
	admissionEffortPattern    = regexp.MustCompile(`^\s*[-*+]\s+(?:\*\*([^*]+)\*\*|` + "`([^`]+)`" + `)\s*(?:[—–:-]\s*)?(.+?)\s*$`)
	admissionEffortValue      = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

type BacklogAdmission struct {
	Enabled                bool                    `yaml:"enabled"`
	Schedule               string                  `yaml:"schedule"`
	Sources                BacklogAdmissionSources `yaml:"sources"`
	TargetState            string                  `yaml:"target_state"`
	CriteriaSection        string                  `yaml:"criteria_section"`
	RequireEffort          bool                    `yaml:"require_effort"`
	EffortFile             string                  `yaml:"effort_file,omitempty"`
	EffortSection          string                  `yaml:"effort_section,omitempty"`
	ExcludeLabels          []string                `yaml:"exclude_labels,omitempty"`
	Authors                BacklogAdmissionAuthors `yaml:"authors,omitempty"`
	MaxCandidatesPerRun    int                     `yaml:"max_candidates_per_run"`
	MaxProposalsPerRun     int                     `yaml:"max_proposals_per_run"`
	MaxOpenProposals       int                     `yaml:"max_open_proposals"`
	ProposalExpiryDays     int                     `yaml:"proposal_expiry_days"`
	AutoAdmit              bool                    `yaml:"auto_admit"`
	AutoAdmitByLabel       map[string]bool         `yaml:"auto_admit_by_label,omitempty"`
	AutoAdmitMinConfidence float64                 `yaml:"auto_admit_min_confidence"`
}

type BacklogAdmissionSources struct {
	States    []string `yaml:"states"`
	Labels    []string `yaml:"labels,omitempty"`
	Untracked bool     `yaml:"untracked,omitempty"`
}

type BacklogAdmissionAuthors struct {
	Allow            []string `yaml:"allow,omitempty"`
	AllowAssociation []string `yaml:"allow_association,omitempty"`
}

type AdmissionCriteria struct {
	Section    string
	Text       string
	Dimensions []AdmissionDimension
}

type AdmissionDimension struct {
	Name string
	Text string
}

type AdmissionEffortRubric struct {
	Section string
	Text    string
	Efforts []string
}

func ValidAdmissionEffortValue(value string) bool {
	return admissionEffortValue.MatchString(strings.TrimSpace(value))
}

func (a *BacklogAdmission) Normalize() {
	if a == nil {
		return
	}
	a.Schedule = strings.TrimSpace(a.Schedule)
	if a.Schedule == "" {
		a.Schedule = DefaultBacklogAdmissionSchedule
	}
	a.Sources.States = normalizeStateList(a.Sources.States)
	a.Sources.Labels = normalizeAdmissionSourceLabels(a.Sources.Labels)
	a.TargetState = strings.TrimSpace(a.TargetState)
	a.CriteriaSection = strings.TrimSpace(a.CriteriaSection)
	a.EffortFile = normalizeAdmissionEffortFile(a.EffortFile)
	a.EffortSection = strings.TrimSpace(a.EffortSection)
	a.ExcludeLabels = normalizeLabels(a.ExcludeLabels)
	a.AutoAdmitByLabel = normalizeAdmissionLabelPolicies(a.AutoAdmitByLabel)
	a.Authors.Allow = normalizeAdmissionAuthors(a.Authors.Allow)
	a.Authors.AllowAssociation = normalizeAdmissionAssociations(a.Authors.AllowAssociation)
}

func (a BacklogAdmission) Validate(prefix string, states []string, tracker Tracker) []string {
	if !a.Enabled {
		return nil
	}
	if prefix == "" {
		prefix = "backlog_admission"
	}
	a.Normalize()
	problems := []string{}
	if _, err := cron.ParseStandard(a.Schedule); err != nil {
		problems = append(problems, prefix+".schedule must be a valid five-field cron expression")
	}
	if len(a.Sources.States) == 0 && len(a.Sources.Labels) == 0 && !a.Sources.Untracked {
		problems = append(problems, prefix+".sources must configure at least one selector")
	}
	known := make(map[string]string, len(states))
	for _, state := range states {
		known[strings.ToLower(strings.TrimSpace(state))] = strings.TrimSpace(state)
	}
	for index, state := range a.Sources.States {
		if _, ok := known[strings.ToLower(state)]; !ok {
			problems = append(problems, fmt.Sprintf("%s.sources.states[%d] must name a configured workflow state", prefix, index))
		}
		if strings.EqualFold(state, a.TargetState) {
			problems = append(problems, fmt.Sprintf("%s.sources.states[%d] must differ from target_state", prefix, index))
		}
	}
	if a.TargetState == "" {
		problems = append(problems, prefix+".target_state is required")
	} else if _, ok := known[strings.ToLower(a.TargetState)]; !ok {
		problems = append(problems, prefix+".target_state must name a configured workflow state")
	}
	if a.CriteriaSection == "" {
		problems = append(problems, prefix+".criteria_section is required")
	}
	if a.RequireEffort && a.EffortSection == "" {
		problems = append(problems, prefix+".effort_section is required when require_effort is true")
	}
	if a.EffortFile != BacklogAdmissionEffortFileWorkflow && a.EffortFile != BacklogAdmissionEffortFileAgents {
		problems = append(problems, prefix+".effort_file must be WORKFLOW.md or AGENTS.md")
	}
	validatePositive(prefix+".max_candidates_per_run", a.MaxCandidatesPerRun, &problems)
	validatePositive(prefix+".max_proposals_per_run", a.MaxProposalsPerRun, &problems)
	validatePositive(prefix+".max_open_proposals", a.MaxOpenProposals, &problems)
	validatePositive(prefix+".proposal_expiry_days", a.ProposalExpiryDays, &problems)
	if a.autoAdmitEnabled() && (a.AutoAdmitMinConfidence < 0 || a.AutoAdmitMinConfidence > 1) {
		problems = append(problems, prefix+".auto_admit_min_confidence must be between 0 and 1")
	}
	for label := range a.AutoAdmitByLabel {
		if strings.TrimSpace(label) == "" {
			problems = append(problems, prefix+".auto_admit_by_label labels must not be blank")
			break
		}
	}
	capabilities := connector.CandidateCapabilitiesFor(connector.Backend(tracker.Kind), tracker.GitHubStatusSource)
	for index, association := range a.Authors.AllowAssociation {
		if !connector.NormalizeAuthorAssociation(association).Valid() {
			problems = append(problems, fmt.Sprintf("%s.authors.allow_association[%d] must be one of OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, NONE", prefix, index))
		}
	}
	if len(a.Authors.AllowAssociation) > 0 && !capabilities.AuthorAssociation {
		gap := prefix + ".authors.allow_association requires author association, but tracker.kind " + tracker.Kind
		if tracker.Kind == TrackerGitHub {
			gap += " with github_status_source " + tracker.GitHubStatusSource
		}
		gap += " cannot supply it"
		problems = append(problems, gap)
	}
	if len(a.Sources.States) > 0 && !capabilities.Supports(connector.CandidateSelectorStates) {
		gap := prefix + ".sources.states requires candidate selector states, but tracker.kind " + tracker.Kind
		if tracker.Kind == TrackerGitHub {
			gap += " with github_status_source " + tracker.GitHubStatusSource
		}
		if tracker.Kind == TrackerLinear {
			gap += " cannot serve it because FetchIssuesByStates is not implemented"
		} else {
			gap += " does not declare it"
		}
		problems = append(problems, gap)
	}
	statusLabelPrefix := strings.TrimSpace(tracker.StatusLabelPrefix)
	if statusLabelPrefix == "" {
		statusLabelPrefix = "detent:"
	}
	for index, label := range a.Sources.Labels {
		if strings.HasPrefix(strings.ToLower(label), strings.ToLower(statusLabelPrefix)) {
			problems = append(problems, fmt.Sprintf(
				"%s.sources.labels[%d] must not use status label prefix %q; use sources.states instead",
				prefix,
				index,
				statusLabelPrefix,
			))
		}
	}
	if len(a.Sources.Labels) > 0 && !capabilities.Supports(connector.CandidateSelectorLabels) {
		gap := prefix + ".sources.labels requires candidate selector labels, but tracker.kind " + tracker.Kind
		if tracker.Kind == TrackerGitHub {
			gap += " with github_status_source " + tracker.GitHubStatusSource
		}
		gap += " does not declare complete label reads"
		problems = append(problems, gap)
	}
	if len(a.ExcludeLabels) > 0 && tracker.Kind == TrackerGitHub && tracker.GitHubStatusSource == GitHubStatusSourceProjectV2 {
		problems = append(problems, prefix+".exclude_labels requires complete issue labels, but tracker.kind github with github_status_source project_v2 fetches only the first 20 labels")
	}
	if len(a.AutoAdmitByLabel) > 0 && tracker.Kind == TrackerGitHub && tracker.GitHubStatusSource == GitHubStatusSourceProjectV2 {
		problems = append(problems, prefix+".auto_admit_by_label requires complete issue labels, but tracker.kind github with github_status_source project_v2 fetches only the first 20 labels")
	}
	if a.Sources.Untracked && !capabilities.Supports(connector.CandidateSelectorUntracked) {
		gap := prefix + ".sources.untracked requires candidate selector untracked, but tracker.kind " + tracker.Kind
		switch tracker.Kind {
		case TrackerGitHub:
			gap += " with github_status_source " + tracker.GitHubStatusSource +
				" cannot serve it because untracked issues are only defined for github label status"
		case TrackerGitHubLocal:
			gap += " cannot serve it because github_local status drift does not populate UntrackedOpen"
		default:
			gap += " does not provide github label status drift"
		}
		problems = append(problems, gap)
	}
	return problems
}

func (a BacklogAdmission) AutoAdmitForLabels(labels []string) bool {
	matched := false
	for _, label := range labels {
		value, ok := a.AutoAdmitByLabel[normalizeLabel(label)]
		if !ok {
			continue
		}
		if !value {
			return false
		}
		matched = true
	}
	if matched {
		return true
	}
	return a.AutoAdmit
}

func (a BacklogAdmission) autoAdmitEnabled() bool {
	if a.AutoAdmit {
		return true
	}
	for _, enabled := range a.AutoAdmitByLabel {
		if enabled {
			return true
		}
	}
	return false
}

func BacklogAdmissionWarnings(admission BacklogAdmission, tracker Tracker) []string {
	if !admission.Enabled {
		return nil
	}
	admission.Normalize()
	warnings := []string{}
	if len(admission.Authors.Allow) > 0 && (tracker.Kind == TrackerLocalSQLite || tracker.Kind == TrackerMemory) {
		warnings = append(warnings, "backlog_admission.authors.allow uses AuthorID, but tracker.kind "+tracker.Kind+" does not discover authors")
	}
	if warning := BacklogAdmissionBotExclusionWarning(admission.Authors, admission.Sources.Untracked); warning != "" {
		warnings = append(warnings, warning)
	}
	if tracker.Kind == TrackerMemory {
		warnings = append(warnings, "backlog_admission with tracker.kind memory is evaluation-only across restarts because tracker comments and mutations are process-local")
	}
	return warnings
}

func normalizeAdmissionSourceLabels(labels []string) []string {
	normalized := normalizeLabels(labels)
	out := make([]string, 0, len(normalized))
	for _, label := range normalized {
		if label != "" {
			out = append(out, label)
		}
	}
	return out
}

func normalizeAdmissionLabelPolicies(policies map[string]bool) map[string]bool {
	normalized := make(map[string]bool, len(policies))
	for label, enabled := range policies {
		label = normalizeLabel(label)
		current, exists := normalized[label]
		if !exists || current {
			normalized[label] = enabled
		}
	}
	return normalized
}

func BacklogAdmissionPublicExposureWarning(admission BacklogAdmission, visibility string) string {
	if !admission.Enabled || !strings.EqualFold(strings.TrimSpace(visibility), "public") {
		return ""
	}
	admission.Normalize()
	if !admissionAllowsUntrustedAuthors(admission.Authors) {
		return ""
	}
	return "backlog_admission on a public repository allows untrusted issue authors to reach the candidate set; configure authors.allow and/or trusted authors.allow_association values"
}

func BacklogAdmissionBotExclusionWarning(authors BacklogAdmissionAuthors, untracked bool) string {
	if !untracked || len(authors.AllowAssociation) == 0 || len(authors.Allow) > 0 {
		return ""
	}
	return "backlog_admission untracked selection with authors.allow_association and an empty authors.allow excludes integration accounts unless each bot handle is allowlisted"
}

func admissionAllowsUntrustedAuthors(authors BacklogAdmissionAuthors) bool {
	if len(authors.Allow) == 0 && len(authors.AllowAssociation) == 0 {
		return true
	}
	for _, value := range authors.AllowAssociation {
		switch connector.NormalizeAuthorAssociation(value) {
		case connector.AuthorAssociationContributor,
			connector.AuthorAssociationFirstTimeContributor,
			connector.AuthorAssociationNone:
			return true
		}
	}
	return false
}

func ResolveAdmissionCriteria(prompt string, section string) (AdmissionCriteria, error) {
	section = strings.TrimSpace(section)
	if section == "" {
		return AdmissionCriteria{}, errors.New("backlog admission criteria section is required")
	}
	match, text, err := resolveAdmissionSection(prompt, section, "criteria", BacklogAdmissionEffortFileWorkflow)
	if err != nil {
		return AdmissionCriteria{}, err
	}
	dimensions, err := admissionDimensions(text, match.Level)
	if err != nil {
		return AdmissionCriteria{}, err
	}
	if len(dimensions) == 0 {
		return AdmissionCriteria{}, fmt.Errorf("backlog admission criteria section %q must define at least one dimension using a bold list item or nested heading", section)
	}
	return AdmissionCriteria{Section: match.Title, Text: text, Dimensions: dimensions}, nil
}

func ResolveAdmissionEffortRubric(prompt string, section string) (AdmissionEffortRubric, error) {
	return resolveAdmissionEffortRubric(prompt, section, BacklogAdmissionEffortFileWorkflow)
}

func ResolveWorkflowAdmissionEffortRubric(workflow Workflow) (AdmissionEffortRubric, error) {
	file := normalizeAdmissionEffortFile(workflow.Config.BacklogAdmission.EffortFile)
	prompt := workflow.SharedPrompt
	alternatePrompt := workflow.AgentsPrompt
	alternateFile := BacklogAdmissionEffortFileAgents
	switch file {
	case BacklogAdmissionEffortFileWorkflow:
	case BacklogAdmissionEffortFileAgents:
		prompt = workflow.AgentsPrompt
		alternatePrompt = workflow.SharedPrompt
		alternateFile = BacklogAdmissionEffortFileWorkflow
	default:
		return AdmissionEffortRubric{}, fmt.Errorf("backlog admission effort file %q must be WORKFLOW.md or AGENTS.md", file)
	}

	rubric, err := resolveAdmissionEffortRubric(prompt, workflow.Config.BacklogAdmission.EffortSection, file)
	if err == nil || !admissionHeadingExists(alternatePrompt, workflow.Config.BacklogAdmission.EffortSection) {
		return rubric, err
	}
	return AdmissionEffortRubric{}, fmt.Errorf(
		"%w; section %q exists in %s instead; set backlog_admission.effort_file to %s or move the section",
		err,
		strings.TrimSpace(workflow.Config.BacklogAdmission.EffortSection),
		alternateFile,
		alternateFile,
	)
}

func resolveAdmissionEffortRubric(prompt string, section string, source string) (AdmissionEffortRubric, error) {
	section = strings.TrimSpace(section)
	if section == "" {
		return AdmissionEffortRubric{}, errors.New("backlog admission effort section is required")
	}
	match, text, err := resolveAdmissionSection(prompt, section, "effort", source)
	if err != nil {
		return AdmissionEffortRubric{}, err
	}
	efforts, err := admissionEfforts(text)
	if err != nil {
		return AdmissionEffortRubric{}, err
	}
	if len(efforts) == 0 {
		return AdmissionEffortRubric{}, fmt.Errorf("backlog admission effort section %q must define at least one effort using a bold or code-formatted list item", section)
	}
	return AdmissionEffortRubric{Section: match.Title, Text: text, Efforts: efforts}, nil
}

func resolveAdmissionSection(prompt string, section string, kind string, source string) (markdownHeading, string, error) {
	headings := markdownHeadings(prompt)
	matches := make([]markdownHeading, 0, 1)
	for _, heading := range headings {
		if strings.EqualFold(strings.TrimSpace(heading.Title), section) {
			matches = append(matches, heading)
		}
	}
	if len(matches) == 0 {
		return markdownHeading{}, "", fmt.Errorf("backlog admission %s section %q was not found in %s", kind, section, source)
	}
	if len(matches) > 1 {
		return markdownHeading{}, "", fmt.Errorf("backlog admission %s section %q is duplicated in %s", kind, section, source)
	}
	match := matches[0]
	end := len(prompt)
	for _, heading := range headings {
		if heading.Start <= match.Start || heading.Level > match.Level {
			continue
		}
		end = heading.Start
		break
	}
	text := strings.TrimSpace(prompt[match.End:end])
	if text == "" {
		return markdownHeading{}, "", fmt.Errorf("backlog admission %s section %q is empty in %s", kind, section, source)
	}
	return match, text, nil
}

func ValidateWorkflowAdmission(workflow Workflow) error {
	if !workflow.Config.BacklogAdmission.Enabled {
		return nil
	}
	if _, err := ResolveAdmissionCriteria(workflow.SharedPrompt, workflow.Config.BacklogAdmission.CriteriaSection); err != nil {
		return err
	}
	if !workflow.Config.BacklogAdmission.RequireEffort {
		return nil
	}
	_, err := ResolveWorkflowAdmissionEffortRubric(workflow)
	return err
}

func normalizeAdmissionEffortFile(file string) string {
	switch strings.ToLower(strings.TrimSpace(file)) {
	case "", "workflow.md":
		return BacklogAdmissionEffortFileWorkflow
	case "agents.md":
		return BacklogAdmissionEffortFileAgents
	default:
		return strings.TrimSpace(file)
	}
}

func admissionHeadingExists(prompt string, section string) bool {
	section = strings.TrimSpace(section)
	for _, heading := range markdownHeadings(prompt) {
		if strings.EqualFold(strings.TrimSpace(heading.Title), section) {
			return true
		}
	}
	return false
}

type markdownHeading struct {
	Title string
	Level int
	Start int
	End   int
}

func markdownHeadings(markdown string) []markdownHeading {
	lines := markdownLines(markdown)
	codeFenceLines := markdownCodeFenceLines(lines)
	headings := []markdownHeading{}
	for index, line := range lines {
		if codeFenceLines[index] {
			continue
		}
		if level, title, ok := atxHeading(line.Text); ok {
			headings = append(headings, markdownHeading{Title: title, Level: level, Start: line.Start, End: line.End})
			continue
		}
		if index == 0 || codeFenceLines[index-1] {
			continue
		}
		level, ok := setextLevel(line.Text)
		if !ok {
			continue
		}
		previous := lines[index-1]
		if markdownIndent(previous.Text) > 3 {
			continue
		}
		title := strings.TrimSpace(previous.Text)
		if title == "" {
			continue
		}
		headings = append(headings, markdownHeading{Title: title, Level: level, Start: previous.Start, End: line.End})
	}
	return headings
}

type markdownLine struct {
	Text  string
	Start int
	End   int
}

func markdownLines(markdown string) []markdownLine {
	lines := []markdownLine{}
	for start := 0; start <= len(markdown); {
		offset := strings.IndexByte(markdown[start:], '\n')
		end := len(markdown)
		next := len(markdown) + 1
		if offset >= 0 {
			end = start + offset
			next = end + 1
		}
		lines = append(lines, markdownLine{Text: strings.TrimSuffix(markdown[start:end], "\r"), Start: start, End: min(next, len(markdown))})
		if next > len(markdown) {
			break
		}
		start = next
	}
	return lines
}

func atxHeading(line string) (int, string, bool) {
	if markdownIndent(line) > 3 {
		return 0, "", false
	}
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "#") {
		return 0, "", false
	}
	level := 0
	for level < len(trimmed) && level < 6 && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level >= len(trimmed) || (trimmed[level] != ' ' && trimmed[level] != '\t') {
		return 0, "", false
	}
	title := strings.TrimSpace(trimmed[level:])
	hashStart := len(title)
	for hashStart > 0 && title[hashStart-1] == '#' {
		hashStart--
	}
	if hashStart > 0 && hashStart < len(title) && (title[hashStart-1] == ' ' || title[hashStart-1] == '\t') {
		title = strings.TrimSpace(title[:hashStart])
	}
	if title == "" {
		return 0, "", false
	}
	return level, title, true
}

func setextLevel(line string) (int, bool) {
	if markdownIndent(line) > 3 {
		return 0, false
	}
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 3 {
		return 0, false
	}
	switch {
	case strings.Trim(trimmed, "=") == "":
		return 1, true
	case strings.Trim(trimmed, "-") == "":
		return 2, true
	default:
		return 0, false
	}
}

func admissionDimensions(text string, sectionLevel int) ([]AdmissionDimension, error) {
	headings := markdownHeadings(text)
	dimensions := []AdmissionDimension{}
	for index, heading := range headings {
		if heading.Level <= sectionLevel {
			continue
		}
		end := len(text)
		for _, next := range headings[index+1:] {
			if next.Level <= heading.Level {
				end = next.Start
				break
			}
		}
		dimensionText := strings.TrimSpace(text[heading.Start:end])
		dimensions = append(dimensions, AdmissionDimension{Name: strings.TrimSpace(heading.Title), Text: dimensionText})
	}
	lines := markdownLines(text)
	codeFenceLines := markdownCodeFenceLines(lines)
	for index, line := range lines {
		if codeFenceLines[index] {
			continue
		}
		match := admissionDimensionPattern.FindStringSubmatch(line.Text)
		if len(match) == 0 {
			continue
		}
		dimensions = append(dimensions, AdmissionDimension{Name: strings.TrimSpace(match[1]), Text: strings.TrimSpace(line.Text)})
	}
	seen := map[string]struct{}{}
	out := make([]AdmissionDimension, 0, len(dimensions))
	for _, dimension := range dimensions {
		key := strings.ToLower(strings.TrimSpace(dimension.Name))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("backlog admission criteria dimension %q is duplicated", dimension.Name)
		}
		seen[key] = struct{}{}
		out = append(out, dimension)
	}
	return out, nil
}

func admissionEfforts(text string) ([]string, error) {
	lines := markdownLines(text)
	codeFenceLines := markdownCodeFenceLines(lines)
	efforts := []string{}
	seen := map[string]struct{}{}
	for index, line := range lines {
		if codeFenceLines[index] {
			continue
		}
		match := admissionEffortPattern.FindStringSubmatch(line.Text)
		if len(match) == 0 {
			continue
		}
		effort := strings.TrimSpace(match[1])
		if effort == "" {
			effort = strings.TrimSpace(match[2])
		}
		key := strings.ToLower(effort)
		if key == "" {
			continue
		}
		if !ValidAdmissionEffortValue(effort) {
			return nil, fmt.Errorf("backlog admission effort %q contains unsupported characters", effort)
		}
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("backlog admission effort %q is duplicated", effort)
		}
		seen[key] = struct{}{}
		efforts = append(efforts, effort)
	}
	return efforts, nil
}

func markdownCodeFenceLines(lines []markdownLine) []bool {
	fenced := make([]bool, len(lines))
	var marker byte
	var markerLength int
	for index, line := range lines {
		character, length, rest, ok := markdownFenceMarker(line.Text)
		if marker == 0 {
			if ok {
				marker = character
				markerLength = length
				fenced[index] = true
			}
			continue
		}
		fenced[index] = true
		if ok && character == marker && length >= markerLength && strings.TrimSpace(rest) == "" {
			marker = 0
			markerLength = 0
		}
	}
	return fenced
}

func markdownFenceMarker(line string) (byte, int, string, bool) {
	if markdownIndent(line) > 3 {
		return 0, 0, "", false
	}
	trimmed := strings.TrimLeft(line, " \t")
	if len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return 0, 0, "", false
	}
	character := trimmed[0]
	length := 0
	for length < len(trimmed) && trimmed[length] == character {
		length++
	}
	if length < 3 {
		return 0, 0, "", false
	}
	return character, length, trimmed[length:], true
}

func markdownIndent(line string) int {
	indent := 0
	for _, character := range line {
		switch character {
		case ' ':
			indent++
		case '\t':
			return 4
		default:
			return indent
		}
	}
	return indent
}

func normalizeAdmissionAuthors(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimPrefix(strings.TrimSpace(value), "@")
		key := strings.ToLower(value)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func normalizeAdmissionAssociations(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = string(connector.NormalizeAuthorAssociation(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized
}
