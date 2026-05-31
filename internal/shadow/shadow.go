package shadow

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/connector"
	"github.com/digitaldrywood/symphony-go/internal/orchestrator"
)

var ErrDifferences = errors.New("shadow run differences found")

type Input struct {
	Date     string       `json:"date"`
	Now      string       `json:"now,omitempty"`
	Scenario *Scenario    `json:"scenario,omitempty"`
	Go       *Observation `json:"go,omitempty"`
	Elixir   Observation  `json:"elixir"`
}

type Scenario struct {
	Config       DispatchConfig  `json:"config"`
	InitialState InitialState    `json:"initial_state,omitempty"`
	Candidates   []Issue         `json:"candidates,omitempty"`
	Tokens       TokenAccounting `json:"tokens,omitempty"`
}

type DispatchConfig struct {
	MaxConcurrentAgents          int            `json:"max_concurrent_agents"`
	MaxConcurrentAgentsByState   map[string]int `json:"max_concurrent_agents_by_state,omitempty"`
	DispatchPriorityByState      []string       `json:"dispatch_priority_by_state,omitempty"`
	ActiveStates                 []string       `json:"active_states,omitempty"`
	TerminalStates               []string       `json:"terminal_states,omitempty"`
	BudgetRefusalCooldownSeconds int            `json:"budget_refusal_cooldown_seconds,omitempty"`
	MaxConcurrentAgentsPerHost   int            `json:"max_concurrent_agents_per_host,omitempty"`
	WorkerHosts                  []string       `json:"worker_hosts,omitempty"`
}

type InitialState struct {
	Running        []Running       `json:"running,omitempty"`
	Claimed        []Claimed       `json:"claimed,omitempty"`
	Blocked        []Blocked       `json:"blocked,omitempty"`
	Retry          []Retry         `json:"retry,omitempty"`
	BudgetRefusals []BudgetRefusal `json:"budget_refusals,omitempty"`
}

type Running struct {
	Issue      Issue  `json:"issue"`
	Attempt    int    `json:"attempt,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	WorkerHost string `json:"worker_host,omitempty"`
}

type Claimed struct {
	Issue     Issue  `json:"issue"`
	ClaimedAt string `json:"claimed_at,omitempty"`
}

type Blocked struct {
	Issue     Issue  `json:"issue"`
	Reason    string `json:"reason,omitempty"`
	BlockedAt string `json:"blocked_at,omitempty"`
}

type Retry struct {
	Issue      Issue  `json:"issue"`
	Attempt    int    `json:"attempt,omitempty"`
	DueAt      string `json:"due_at,omitempty"`
	Error      string `json:"error,omitempty"`
	WorkerHost string `json:"worker_host,omitempty"`
}

type BudgetRefusal struct {
	Issue     Issue  `json:"issue"`
	RefusedAt string `json:"refused_at,omitempty"`
	ResetAt   string `json:"reset_at,omitempty"`
}

type Issue struct {
	ID               string                 `json:"id"`
	Identifier       string                 `json:"identifier,omitempty"`
	URL              string                 `json:"url,omitempty"`
	Title            string                 `json:"title,omitempty"`
	State            string                 `json:"state,omitempty"`
	Priority         *int                   `json:"priority,omitempty"`
	CreatedAt        string                 `json:"created_at,omitempty"`
	UpdatedAt        string                 `json:"updated_at,omitempty"`
	BlockedBy        []connector.BlockedRef `json:"blocked_by,omitempty"`
	Labels           []string               `json:"labels,omitempty"`
	AssignedToWorker *bool                  `json:"assigned_to_worker,omitempty"`
	ModelOverride    string                 `json:"model_override,omitempty"`
	BranchName       string                 `json:"branch_name,omitempty"`
}

type Observation struct {
	Dispatch DispatchObservation `json:"dispatch"`
	Tokens   TokenAccounting     `json:"tokens"`
}

type DispatchObservation struct {
	DispatchOrder  []string         `json:"dispatch_order,omitempty"`
	Dispatches     []DispatchDetail `json:"dispatches,omitempty"`
	Claimed        []string         `json:"claimed,omitempty"`
	Blocked        []string         `json:"blocked,omitempty"`
	BudgetRefusals []string         `json:"budget_refusals,omitempty"`
	Refusals       []string         `json:"refusals,omitempty"`
	Retry          []string         `json:"retry,omitempty"`
}

type DispatchDetail struct {
	IssueID    string `json:"issue_id"`
	Identifier string `json:"identifier,omitempty"`
	State      string `json:"state,omitempty"`
	Attempt    int    `json:"attempt,omitempty"`
	WorkerHost string `json:"worker_host,omitempty"`
	Retry      bool   `json:"retry,omitempty"`
}

type TokenAccounting struct {
	InputTokens    int64                  `json:"input_tokens"`
	OutputTokens   int64                  `json:"output_tokens"`
	TotalTokens    int64                  `json:"total_tokens"`
	Sessions       int64                  `json:"sessions,omitempty"`
	RuntimeSeconds int64                  `json:"runtime_seconds,omitempty"`
	ByModel        []ModelTokenAccounting `json:"by_model,omitempty"`
}

type ModelTokenAccounting struct {
	Model          string `json:"model,omitempty"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	TotalTokens    int64  `json:"total_tokens"`
	Sessions       int64  `json:"sessions,omitempty"`
	RuntimeSeconds int64  `json:"runtime_seconds,omitempty"`
}

type Report struct {
	Date        string      `json:"date"`
	GeneratedAt time.Time   `json:"generated_at"`
	Go          Observation `json:"go"`
	Elixir      Observation `json:"elixir"`
	Diff        Diff        `json:"diff"`
}

type Diff struct {
	HasDifferences bool             `json:"has_differences"`
	Dispatch       []DispatchDiff   `json:"dispatch,omitempty"`
	Tokens         []TokenDiff      `json:"tokens,omitempty"`
	ModelTokens    []ModelTokenDiff `json:"model_tokens,omitempty"`
}

type DispatchDiff struct {
	Field  string   `json:"field"`
	Go     []string `json:"go"`
	Elixir []string `json:"elixir"`
}

type TokenDiff struct {
	Field  string `json:"field"`
	Go     int64  `json:"go"`
	Elixir int64  `json:"elixir"`
}

type ModelTokenDiff struct {
	Model  string `json:"model"`
	Field  string `json:"field"`
	Go     int64  `json:"go"`
	Elixir int64  `json:"elixir"`
}

func Run(input Input) (Report, error) {
	generatedAt, err := reportTime(input.Date, input.Now)
	if err != nil {
		return Report{}, err
	}

	goObservation, err := goObservation(input, generatedAt)
	if err != nil {
		return Report{}, err
	}

	date := strings.TrimSpace(input.Date)
	if date == "" {
		date = generatedAt.Format("2006-01-02")
	}

	goObservation = normalizeObservation(goObservation)
	elixirObservation := normalizeObservation(input.Elixir)
	diff := compareObservations(goObservation, elixirObservation)

	return Report{
		Date:        date,
		GeneratedAt: generatedAt,
		Go:          goObservation,
		Elixir:      elixirObservation,
		Diff:        diff,
	}, nil
}

func (r Report) HasDifferences() bool {
	return r.Diff.HasDifferences
}

func goObservation(input Input, now time.Time) (Observation, error) {
	if input.Scenario != nil {
		return input.Scenario.observation(now)
	}
	if input.Go == nil {
		return Observation{}, errors.New("go observation or scenario is required")
	}
	return *input.Go, nil
}

func (s Scenario) observation(now time.Time) (Observation, error) {
	cfg := s.Config.orchestratorConfig()
	state, err := s.InitialState.orchestratorState(cfg, now)
	if err != nil {
		return Observation{}, err
	}

	candidates := make([]connector.Issue, 0, len(s.Candidates))
	for _, candidate := range s.Candidates {
		issue, err := candidate.connectorIssue()
		if err != nil {
			return Observation{}, err
		}
		candidates = append(candidates, issue)
	}

	plan := orchestrator.PlanDispatch(cfg, state, candidates, now)
	return Observation{
		Dispatch: dispatchObservationFromPlan(plan),
		Tokens:   s.Tokens,
	}, nil
}

func (c DispatchConfig) orchestratorConfig() orchestrator.Config {
	return orchestrator.Config{
		MaxConcurrentAgents:        c.MaxConcurrentAgents,
		MaxConcurrentAgentsByState: cloneIntMap(c.MaxConcurrentAgentsByState),
		DispatchPriorityByState:    append([]string(nil), c.DispatchPriorityByState...),
		ActiveStates:               append([]string(nil), c.ActiveStates...),
		TerminalStates:             append([]string(nil), c.TerminalStates...),
		WorkerHosts:                append([]string(nil), c.WorkerHosts...),
		MaxConcurrentAgentsPerHost: c.MaxConcurrentAgentsPerHost,
		BudgetRefusalCooldown:      time.Duration(c.BudgetRefusalCooldownSeconds) * time.Second,
	}
}

func (s InitialState) orchestratorState(cfg orchestrator.Config, now time.Time) (orchestrator.State, error) {
	state := orchestrator.State{
		MaxConcurrentAgents: cfg.MaxConcurrentAgents,
		Running:             map[string]orchestrator.Running{},
		Claimed:             map[string]orchestrator.Claimed{},
		Blocked:             map[string]orchestrator.Blocked{},
		Completed:           map[string]orchestrator.Completed{},
		Retry:               map[string]orchestrator.Retry{},
		BudgetRefusals:      map[string]orchestrator.BudgetRefusal{},
		DiffStats:           map[string]orchestrator.DiffStats{},
	}

	for _, row := range s.Running {
		issue, err := row.Issue.connectorIssue()
		if err != nil {
			return orchestrator.State{}, err
		}
		state.Running[issue.ID] = orchestrator.Running{
			Issue:      issue,
			Attempt:    row.Attempt,
			StartedAt:  optionalTime(row.StartedAt, now),
			WorkerHost: strings.TrimSpace(row.WorkerHost),
		}
	}
	for _, row := range s.Claimed {
		issue, err := row.Issue.connectorIssue()
		if err != nil {
			return orchestrator.State{}, err
		}
		state.Claimed[issue.ID] = orchestrator.Claimed{
			Issue:     issue,
			ClaimedAt: optionalTime(row.ClaimedAt, now),
		}
	}
	for _, row := range s.Blocked {
		issue, err := row.Issue.connectorIssue()
		if err != nil {
			return orchestrator.State{}, err
		}
		state.Blocked[issue.ID] = orchestrator.Blocked{
			Issue:     issue,
			Reason:    strings.TrimSpace(row.Reason),
			BlockedAt: optionalTime(row.BlockedAt, now),
		}
	}
	for _, row := range s.Retry {
		issue, err := row.Issue.connectorIssue()
		if err != nil {
			return orchestrator.State{}, err
		}
		state.Retry[issue.ID] = orchestrator.Retry{
			Issue:      issue,
			Attempt:    row.Attempt,
			DueAt:      optionalTime(row.DueAt, now),
			Error:      strings.TrimSpace(row.Error),
			WorkerHost: strings.TrimSpace(row.WorkerHost),
		}
	}
	for _, row := range s.BudgetRefusals {
		issue, err := row.Issue.connectorIssue()
		if err != nil {
			return orchestrator.State{}, err
		}
		resetAt := optionalTimePtr(row.ResetAt)
		state.BudgetRefusals[issue.ID] = orchestrator.BudgetRefusal{
			Issue:     issue,
			RefusedAt: optionalTime(row.RefusedAt, now),
			ResetAt:   resetAt,
		}
	}

	return state, nil
}

func (i Issue) connectorIssue() (connector.Issue, error) {
	issue := connector.NewIssue()
	issue.ID = strings.TrimSpace(i.ID)
	issue.Identifier = strings.TrimSpace(i.Identifier)
	issue.URL = strings.TrimSpace(i.URL)
	issue.Title = strings.TrimSpace(i.Title)
	issue.State = strings.TrimSpace(i.State)
	issue.Priority = i.Priority
	issue.BlockedBy = append([]connector.BlockedRef(nil), i.BlockedBy...)
	issue.Labels = trimStrings(i.Labels)
	issue.ModelOverride = strings.TrimSpace(i.ModelOverride)
	issue.BranchName = strings.TrimSpace(i.BranchName)
	issue.AssignedToWorker = true
	if i.AssignedToWorker != nil {
		issue.AssignedToWorker = *i.AssignedToWorker
	}

	if i.CreatedAt != "" {
		createdAt, err := parseTime("created_at", i.CreatedAt)
		if err != nil {
			return connector.Issue{}, err
		}
		issue.CreatedAt = &createdAt
	}
	if i.UpdatedAt != "" {
		updatedAt, err := parseTime("updated_at", i.UpdatedAt)
		if err != nil {
			return connector.Issue{}, err
		}
		issue.UpdatedAt = &updatedAt
	}
	return issue, nil
}

func dispatchObservationFromPlan(plan orchestrator.DispatchPlan) DispatchObservation {
	dispatches := make([]DispatchDetail, 0, len(plan.Dispatches))
	for _, dispatch := range plan.Dispatches {
		dispatches = append(dispatches, DispatchDetail{
			IssueID:    dispatch.IssueID,
			Identifier: dispatch.Identifier,
			State:      dispatch.State,
			Attempt:    dispatch.Attempt,
			WorkerHost: dispatch.WorkerHost,
			Retry:      dispatch.Retry,
		})
	}

	return DispatchObservation{
		DispatchOrder:  plan.DispatchOrder(),
		Dispatches:     dispatches,
		Claimed:        append([]string(nil), plan.Claimed...),
		Blocked:        append([]string(nil), plan.Blocked...),
		BudgetRefusals: append([]string(nil), plan.BudgetRefusals...),
		Retry:          append([]string(nil), plan.Retry...),
	}
}

func normalizeObservation(observation Observation) Observation {
	observation.Dispatch = normalizeDispatchObservation(observation.Dispatch)
	observation.Tokens = normalizeTokenAccounting(observation.Tokens)
	return observation
}

func normalizeDispatchObservation(observation DispatchObservation) DispatchObservation {
	observation.DispatchOrder = normalizeOrderedIDs(observation.DispatchOrder)
	observation.Dispatches = normalizeDispatchDetails(observation.Dispatches)
	if len(observation.DispatchOrder) == 0 && len(observation.Dispatches) > 0 {
		for _, dispatch := range observation.Dispatches {
			observation.DispatchOrder = append(observation.DispatchOrder, dispatch.IssueID)
		}
	}

	observation.Claimed = normalizeIDSet(observation.Claimed)
	observation.Blocked = normalizeIDSet(observation.Blocked)
	observation.BudgetRefusals = normalizeIDSet(append(observation.BudgetRefusals, observation.Refusals...))
	observation.Refusals = nil
	observation.Retry = normalizeIDSet(observation.Retry)
	return observation
}

func normalizeDispatchDetails(dispatches []DispatchDetail) []DispatchDetail {
	normalized := make([]DispatchDetail, 0, len(dispatches))
	for _, dispatch := range dispatches {
		dispatch.IssueID = strings.TrimSpace(dispatch.IssueID)
		dispatch.Identifier = strings.TrimSpace(dispatch.Identifier)
		dispatch.State = strings.TrimSpace(dispatch.State)
		dispatch.WorkerHost = strings.TrimSpace(dispatch.WorkerHost)
		if dispatch.IssueID == "" {
			continue
		}
		normalized = append(normalized, dispatch)
	}
	return normalized
}

func normalizeTokenAccounting(tokens TokenAccounting) TokenAccounting {
	tokens.InputTokens = nonNegative(tokens.InputTokens)
	tokens.OutputTokens = nonNegative(tokens.OutputTokens)
	tokens.TotalTokens = nonNegative(tokens.TotalTokens)
	tokens.Sessions = nonNegative(tokens.Sessions)
	tokens.RuntimeSeconds = nonNegative(tokens.RuntimeSeconds)
	tokens.ByModel = normalizeModelTokens(tokens.ByModel)
	return tokens
}

func normalizeModelTokens(rows []ModelTokenAccounting) []ModelTokenAccounting {
	byModel := map[string]ModelTokenAccounting{}
	for _, row := range rows {
		model := strings.TrimSpace(row.Model)
		existing := byModel[model]
		existing.Model = model
		existing.InputTokens += nonNegative(row.InputTokens)
		existing.OutputTokens += nonNegative(row.OutputTokens)
		existing.TotalTokens += nonNegative(row.TotalTokens)
		existing.Sessions += nonNegative(row.Sessions)
		existing.RuntimeSeconds += nonNegative(row.RuntimeSeconds)
		byModel[model] = existing
	}

	models := make([]string, 0, len(byModel))
	for model := range byModel {
		models = append(models, model)
	}
	sort.Strings(models)

	normalized := make([]ModelTokenAccounting, 0, len(models))
	for _, model := range models {
		normalized = append(normalized, byModel[model])
	}
	return normalized
}

func compareObservations(goObservation Observation, elixirObservation Observation) Diff {
	diff := Diff{}
	diff.Dispatch = dispatchDiffs(goObservation.Dispatch, elixirObservation.Dispatch)
	diff.Tokens = tokenDiffs(goObservation.Tokens, elixirObservation.Tokens)
	diff.ModelTokens = modelTokenDiffs(goObservation.Tokens.ByModel, elixirObservation.Tokens.ByModel)
	diff.HasDifferences = len(diff.Dispatch) > 0 || len(diff.Tokens) > 0 || len(diff.ModelTokens) > 0
	return diff
}

func dispatchDiffs(goDispatch DispatchObservation, elixirDispatch DispatchObservation) []DispatchDiff {
	checks := []struct {
		field string
		goIDs []string
		exIDs []string
	}{
		{field: "dispatch_order", goIDs: goDispatch.DispatchOrder, exIDs: elixirDispatch.DispatchOrder},
		{field: "claimed", goIDs: goDispatch.Claimed, exIDs: elixirDispatch.Claimed},
		{field: "blocked", goIDs: goDispatch.Blocked, exIDs: elixirDispatch.Blocked},
		{field: "budget_refusals", goIDs: goDispatch.BudgetRefusals, exIDs: elixirDispatch.BudgetRefusals},
		{field: "retry", goIDs: goDispatch.Retry, exIDs: elixirDispatch.Retry},
	}

	diffs := make([]DispatchDiff, 0)
	for _, check := range checks {
		if reflect.DeepEqual(check.goIDs, check.exIDs) {
			continue
		}
		diffs = append(diffs, DispatchDiff{
			Field:  check.field,
			Go:     append([]string(nil), check.goIDs...),
			Elixir: append([]string(nil), check.exIDs...),
		})
	}
	return diffs
}

func tokenDiffs(goTokens TokenAccounting, elixirTokens TokenAccounting) []TokenDiff {
	checks := []struct {
		field string
		goVal int64
		exVal int64
	}{
		{field: "input_tokens", goVal: goTokens.InputTokens, exVal: elixirTokens.InputTokens},
		{field: "output_tokens", goVal: goTokens.OutputTokens, exVal: elixirTokens.OutputTokens},
		{field: "total_tokens", goVal: goTokens.TotalTokens, exVal: elixirTokens.TotalTokens},
		{field: "sessions", goVal: goTokens.Sessions, exVal: elixirTokens.Sessions},
		{field: "runtime_seconds", goVal: goTokens.RuntimeSeconds, exVal: elixirTokens.RuntimeSeconds},
	}

	diffs := make([]TokenDiff, 0)
	for _, check := range checks {
		if check.goVal == check.exVal {
			continue
		}
		diffs = append(diffs, TokenDiff{Field: check.field, Go: check.goVal, Elixir: check.exVal})
	}
	return diffs
}

func modelTokenDiffs(goRows []ModelTokenAccounting, elixirRows []ModelTokenAccounting) []ModelTokenDiff {
	goByModel := modelTokenMap(goRows)
	elixirByModel := modelTokenMap(elixirRows)
	models := unionSortedKeys(goByModel, elixirByModel)

	diffs := make([]ModelTokenDiff, 0)
	for _, model := range models {
		goRow := goByModel[model]
		elixirRow := elixirByModel[model]
		checks := []struct {
			field string
			goVal int64
			exVal int64
		}{
			{field: "input_tokens", goVal: goRow.InputTokens, exVal: elixirRow.InputTokens},
			{field: "output_tokens", goVal: goRow.OutputTokens, exVal: elixirRow.OutputTokens},
			{field: "total_tokens", goVal: goRow.TotalTokens, exVal: elixirRow.TotalTokens},
			{field: "sessions", goVal: goRow.Sessions, exVal: elixirRow.Sessions},
			{field: "runtime_seconds", goVal: goRow.RuntimeSeconds, exVal: elixirRow.RuntimeSeconds},
		}
		for _, check := range checks {
			if check.goVal == check.exVal {
				continue
			}
			diffs = append(diffs, ModelTokenDiff{
				Model:  model,
				Field:  check.field,
				Go:     check.goVal,
				Elixir: check.exVal,
			})
		}
	}
	return diffs
}

func modelTokenMap(rows []ModelTokenAccounting) map[string]ModelTokenAccounting {
	byModel := make(map[string]ModelTokenAccounting, len(rows))
	for _, row := range rows {
		byModel[row.Model] = row
	}
	return byModel
}

func unionSortedKeys(left map[string]ModelTokenAccounting, right map[string]ModelTokenAccounting) []string {
	seen := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		seen[key] = struct{}{}
	}
	for key := range right {
		seen[key] = struct{}{}
	}

	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func reportTime(date string, now string) (time.Time, error) {
	if strings.TrimSpace(now) != "" {
		return parseTime("now", now)
	}
	if strings.TrimSpace(date) != "" {
		parsed, err := time.Parse("2006-01-02", strings.TrimSpace(date))
		if err != nil {
			return time.Time{}, fmt.Errorf("parse date: %w", err)
		}
		return parsed, nil
	}
	return time.Now().UTC(), nil
}

func parseTime(name string, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

func optionalTime(value string, fallback time.Time) time.Time {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	parsed, err := parseTime("time", value)
	if err != nil {
		return fallback
	}
	return parsed
}

func optionalTimePtr(value string) *time.Time {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := parseTime("time", value)
	if err != nil {
		return nil
	}
	return &parsed
}

func normalizeOrderedIDs(ids []string) []string {
	normalized := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		normalized = append(normalized, id)
	}
	return normalized
}

func normalizeIDSet(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		seen[id] = struct{}{}
	}

	normalized := make([]string, 0, len(seen))
	for id := range seen {
		normalized = append(normalized, id)
	}
	sort.Strings(normalized)
	return normalized
}

func trimStrings(values []string) []string {
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		trimmed = append(trimmed, value)
	}
	return trimmed
}

func cloneIntMap(values map[string]int) map[string]int {
	cloned := make(map[string]int, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
