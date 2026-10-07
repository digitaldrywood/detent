package operatortool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

var (
	ErrInvalidArguments    = errors.New("invalid tool arguments")
	ErrSnapshotUnavailable = errors.New("operator telemetry snapshot is unavailable")
	ErrUnknownTool         = errors.New("unknown read-only operator tool")
	ErrResultTooLarge      = fmt.Errorf("operator tool result exceeds %d bytes", MaxResultBytes)
)

type Call struct {
	Name      string
	Arguments json.RawMessage
}

type Result struct {
	Content json.RawMessage
}

type SnapshotSource interface {
	Snapshot(context.Context) (telemetry.Snapshot, error)
}

type SnapshotFunc func(context.Context) (telemetry.Snapshot, error)

func (f SnapshotFunc) Snapshot(ctx context.Context) (telemetry.Snapshot, error) {
	return f(ctx)
}

type Explainer interface {
	Explain(context.Context, explain.Query) (explain.IssueExplanation, error)
}

type Dependencies struct {
	Snapshots SnapshotSource
	Explainer Explainer
	WorkReads WorkReader
}

type Executor struct {
	snapshots SnapshotSource
	explainer Explainer
	workReads WorkReader
}

func NewExecutor(deps Dependencies) *Executor {
	return &Executor{snapshots: deps.Snapshots, explainer: deps.Explainer, workReads: deps.WorkReads}
}

func (e *Executor) Execute(ctx context.Context, call Call) (Result, error) {
	if IsWorkRead(call.Name) {
		return e.readWork(ctx, call)
	}
	switch call.Name {
	case BoardState:
		return e.boardState(ctx, call.Arguments)
	case FleetHealth:
		return e.fleetHealth(ctx, call.Arguments)
	case TelemetryUsage:
		return e.telemetryUsage(ctx, call.Arguments)
	case RecentActivity:
		return e.recentActivity(ctx, call.Arguments)
	case ExplainItem:
		return e.explainItem(ctx, call.Arguments)
	default:
		return Result{}, fmt.Errorf("%w %q", ErrUnknownTool, call.Name)
	}
}

type boardStateArguments struct {
	ProjectID string `json:"project_id"`
	State     string `json:"state"`
	Limit     int    `json:"limit"`
}

type telemetryUsageArguments struct {
	ProjectID string `json:"project_id"`
}

type recentActivityArguments struct {
	ProjectID string `json:"project_id"`
	Limit     int    `json:"limit"`
}

type explainItemArguments struct {
	ProjectID string `json:"project_id"`
	Reference string `json:"reference"`
}

func (e *Executor) boardState(ctx context.Context, raw json.RawMessage) (Result, error) {
	var request boardStateArguments
	if err := decodeArguments(raw, &request); err != nil {
		return Result{}, err
	}
	snapshot, err := e.snapshot(ctx)
	if err != nil {
		return Result{}, err
	}
	items := boardItems(snapshot, request.ProjectID, request.State)
	items = bounded(items, itemLimit(request.Limit))
	return encodeResult(BoardStateResult{
		GeneratedAt: snapshot.GeneratedAt,
		Freshness:   snapshotFreshness(snapshot),
		ExpiresAt:   snapshotExpiresAt(snapshot),
		Counts:      snapshot.Counts,
		Items:       items,
	})
}

func (e *Executor) fleetHealth(ctx context.Context, raw json.RawMessage) (Result, error) {
	if err := decodeArguments(raw, &struct{}{}); err != nil {
		return Result{}, err
	}
	snapshot, err := e.snapshot(ctx)
	if err != nil {
		return Result{}, err
	}
	return encodeResult(FleetHealthResult{
		GeneratedAt:        snapshot.GeneratedAt,
		Freshness:          snapshotFreshness(snapshot),
		ExpiresAt:          snapshotExpiresAt(snapshot),
		Auth:               snapshot.Auth,
		Shutdown:           snapshot.Shutdown,
		Refresh:            snapshot.Refresh,
		Counts:             snapshot.Counts,
		RateLimits:         boundedRateLimits(snapshot.RateLimits),
		TrackerUnavailable: boundedClone(snapshot.TrackerUnavailable, MaxItemLimit),
		ForgeUnavailable:   boundedClone(snapshot.ForgeUnavailable, MaxItemLimit),
		BackendOutages:     boundedClone(snapshot.BackendOutages, MaxItemLimit),
		FailureBreakers:    boundedClone(snapshot.FailureBreakers, MaxItemLimit),
		DispatchRecoveries: boundedClone(snapshot.DispatchRecoveries, MaxItemLimit),
	})
}

func (e *Executor) telemetryUsage(ctx context.Context, raw json.RawMessage) (Result, error) {
	var request telemetryUsageArguments
	if err := decodeArguments(raw, &request); err != nil {
		return Result{}, err
	}
	snapshot, err := e.snapshot(ctx)
	if err != nil {
		return Result{}, err
	}
	projects := slices.Clone(snapshot.Projects)
	if request.ProjectID != "" {
		projects = slices.DeleteFunc(projects, func(candidate telemetry.ProjectSnapshot) bool {
			return !strings.EqualFold(candidate.Project.ID, strings.TrimSpace(request.ProjectID))
		})
	}
	projects = bounded(projects, MaxItemLimit)
	return encodeResult(TelemetryUsageResult{
		GeneratedAt:    snapshot.GeneratedAt,
		Freshness:      snapshotFreshness(snapshot),
		ExpiresAt:      snapshotExpiresAt(snapshot),
		Projects:       projects,
		Tokens:         snapshot.Tokens,
		Throughput:     snapshot.Throughput,
		LifetimeTotals: snapshot.LifetimeTotals,
		Budget:         boundedBudget(snapshot.Budget),
	})
}

func (e *Executor) recentActivity(ctx context.Context, raw json.RawMessage) (Result, error) {
	var request recentActivityArguments
	if err := decodeArguments(raw, &request); err != nil {
		return Result{}, err
	}
	snapshot, err := e.snapshot(ctx)
	if err != nil {
		return Result{}, err
	}
	events := slices.Clone(snapshot.Events)
	completed := slices.Clone(snapshot.Completed)
	if request.ProjectID != "" {
		projectID := strings.TrimSpace(request.ProjectID)
		completed = slices.DeleteFunc(completed, func(candidate telemetry.Completed) bool {
			return !strings.EqualFold(candidate.ProjectID, projectID)
		})
	}
	limit := itemLimit(request.Limit)
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	completed = bounded(completed, limit)
	return encodeResult(RecentActivityResult{
		GeneratedAt: snapshot.GeneratedAt,
		Freshness:   snapshotFreshness(snapshot),
		ExpiresAt:   snapshotExpiresAt(snapshot),
		Events:      events,
		Completed:   completed,
	})
}

func (e *Executor) explainItem(ctx context.Context, raw json.RawMessage) (Result, error) {
	var request explainItemArguments
	if err := decodeArguments(raw, &request); err != nil {
		return Result{}, err
	}
	request.ProjectID = strings.TrimSpace(request.ProjectID)
	request.Reference = strings.TrimSpace(request.Reference)
	if request.ProjectID == "" || request.Reference == "" {
		return Result{}, fmt.Errorf("%w: project_id and reference are required", ErrInvalidArguments)
	}
	explainer := e.explainer
	if authority, ok := ctx.Value(authorityKey{}).(Authority); ok && authority.Explainer != nil {
		explainer = authority.Explainer
	}
	if explainer == nil {
		return Result{}, ErrReadUnavailable
	}
	result, err := explainer.Explain(ctx, explain.Query{ProjectID: request.ProjectID, Reference: request.Reference})
	if err != nil {
		return Result{}, err
	}
	if _, authorized := ctx.Value(authorityKey{}).(Authority); authorized && result.Identity.ProjectID != request.ProjectID {
		return Result{}, ErrAccessDenied
	}
	return encodeResult(result)
}

func (e *Executor) snapshot(ctx context.Context) (telemetry.Snapshot, error) {
	if e.snapshots == nil {
		return telemetry.Snapshot{}, ErrSnapshotUnavailable
	}
	snapshot, err := e.snapshots.Snapshot(ctx)
	if err != nil {
		return telemetry.Snapshot{}, err
	}
	if snapshot.GeneratedAt.IsZero() {
		return telemetry.Snapshot{}, ErrSnapshotUnavailable
	}
	return ProjectSnapshot(ctx, snapshot)
}

func decodeArguments(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	if len(raw) > MaxArgumentBytes {
		return invalidArgument("arguments", fmt.Sprintf("must not exceed %d bytes", MaxArgumentBytes))
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var typeError *json.UnmarshalTypeError
		if errors.As(err, &typeError) || strings.HasPrefix(err.Error(), "json: invalid use of ,string struct tag") {
			if detail := quotedArgumentError(raw, reflect.TypeOf(target), ""); detail != nil {
				return detail
			}
		}
		return argumentDecodeError(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return invalidArgument("arguments", "must contain exactly one JSON value")
	}
	return nil
}

func quotedArgumentError(raw json.RawMessage, target reflect.Type, parent string) error {
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target.Kind() != reflect.Struct {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	for i := range target.NumField() {
		field := target.Field(i)
		if !field.IsExported() {
			continue
		}
		parts := strings.Split(field.Tag.Get("json"), ",")
		name := parts[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		value, present := fields[name]
		if !present || string(value) == "null" {
			continue
		}
		path := name
		if parent != "" {
			path = parent + "." + name
		}
		if slices.Contains(parts[1:], "string") && len(value) > 0 && value[0] != '"' {
			return invalidArgument(path, "must be a string")
		}
		if err := quotedArgumentError(value, field.Type, path); err != nil {
			return err
		}
	}
	return nil
}

func argumentDecodeError(err error) error {
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) {
		field := typeError.Field
		if field == "" {
			field = "arguments"
		}
		return invalidArgument(field, "must have type "+typeError.Type.String())
	}
	if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		var name string
		if json.Unmarshal([]byte(field), &name) == nil {
			return invalidArgument(name, "is not an allowed field")
		}
	}
	return invalidArgument("arguments", "must be valid JSON")
}

func snapshotFreshness(snapshot telemetry.Snapshot) explain.SourceState {
	if snapshot.LastKnown {
		return explain.SourceLastKnown
	}
	return explain.SourceLive
}

func snapshotExpiresAt(snapshot telemetry.Snapshot) *time.Time {
	if !snapshot.LastKnown || snapshot.LastKnownUntil.IsZero() {
		return nil
	}
	expiresAt := snapshot.LastKnownUntil.UTC()
	return &expiresAt
}

func encodeResult(value any) (Result, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return Result{}, fmt.Errorf("encode operator tool result: %w", err)
	}
	if len(data) > MaxResultBytes {
		return Result{}, ErrResultTooLarge
	}
	return Result{Content: data}, nil
}

func itemLimit(limit int) int {
	if limit <= 0 {
		return DefaultItemLimit
	}
	return min(limit, MaxItemLimit)
}

func bounded[T any](values []T, limit int) []T {
	if len(values) > limit {
		return values[:limit]
	}
	return values
}

func boundedClone[T any](values []T, limit int) []T {
	return bounded(slices.Clone(values), limit)
}

func boundedRateLimits(rateLimits *telemetry.RateLimits) *telemetry.RateLimits {
	if rateLimits == nil {
		return nil
	}
	result := *rateLimits
	result.GitHubRESTBudgets = boundedClone(rateLimits.GitHubRESTBudgets, MaxItemLimit)
	if rateLimits.GraphQLCost != nil {
		graphql := *rateLimits.GraphQLCost
		graphql.Contributors = boundedClone(graphql.Contributors, MaxItemLimit)
		result.GraphQLCost = &graphql
	}
	if rateLimits.RESTUsage != nil {
		rest := *rateLimits.RESTUsage
		rest.Contributors = boundedClone(rest.Contributors, MaxItemLimit)
		rest.Divergences = boundedClone(rest.Divergences, MaxItemLimit)
		result.RESTUsage = &rest
	}
	return &result
}

func boundedBudget(budget telemetry.Budget) telemetry.Budget {
	budget.SpendPoints = boundedClone(budget.SpendPoints, MaxItemLimit)
	budget.Days = boundedClone(budget.Days, MaxItemLimit)
	budget.Refusals = boundedClone(budget.Refusals, MaxItemLimit)
	return budget
}

type BoardStateResult struct {
	GeneratedAt time.Time           `json:"generated_at"`
	Freshness   explain.SourceState `json:"freshness"`
	ExpiresAt   *time.Time          `json:"expires_at,omitempty"`
	Counts      telemetry.Counts    `json:"counts"`
	Items       []BoardItem         `json:"items"`
}

type BoardItem struct {
	ProjectID         string     `json:"project_id,omitempty"`
	IssueID           string     `json:"issue_id"`
	Identifier        string     `json:"identifier,omitempty"`
	Title             string     `json:"title,omitempty"`
	State             string     `json:"state,omitempty"`
	Priority          *int       `json:"priority,omitempty"`
	PriorityName      string     `json:"priority_name,omitempty"`
	BlockedReason     string     `json:"blocked_reason,omitempty"`
	BlockedSource     string     `json:"blocked_source,omitempty"`
	Active            bool       `json:"active,omitempty"`
	Attempt           int        `json:"attempt,omitempty"`
	WorkAttemptID     int64      `json:"work_attempt_id,omitempty"`
	DetentSessionID   int64      `json:"detent_session_id,omitempty"`
	ProviderSessionID string     `json:"provider_session_id,omitempty"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
}

type FleetHealthResult struct {
	GeneratedAt        time.Time                    `json:"generated_at"`
	Freshness          explain.SourceState          `json:"freshness"`
	ExpiresAt          *time.Time                   `json:"expires_at,omitempty"`
	Auth               telemetry.AuthHealth         `json:"auth"`
	Shutdown           telemetry.Shutdown           `json:"shutdown"`
	Refresh            telemetry.Refresh            `json:"refresh"`
	Counts             telemetry.Counts             `json:"counts"`
	RateLimits         *telemetry.RateLimits        `json:"rate_limits"`
	TrackerUnavailable []telemetry.TrackerCondition `json:"tracker_unavailable"`
	ForgeUnavailable   []telemetry.ForgeCondition   `json:"forge_unavailable"`
	BackendOutages     []telemetry.BackendOutage    `json:"backend_outages"`
	FailureBreakers    []telemetry.FailureBreaker   `json:"failure_breakers"`
	DispatchRecoveries []telemetry.DispatchRecovery `json:"dispatch_recoveries"`
}

type TelemetryUsageResult struct {
	GeneratedAt    time.Time                   `json:"generated_at"`
	Freshness      explain.SourceState         `json:"freshness"`
	ExpiresAt      *time.Time                  `json:"expires_at,omitempty"`
	Projects       []telemetry.ProjectSnapshot `json:"projects"`
	Tokens         telemetry.Tokens            `json:"tokens"`
	Throughput     telemetry.TokenThroughput   `json:"throughput"`
	LifetimeTotals telemetry.LifetimeTotals    `json:"lifetime_totals"`
	Budget         telemetry.Budget            `json:"budget"`
}

type RecentActivityResult struct {
	GeneratedAt time.Time                 `json:"generated_at"`
	Freshness   explain.SourceState       `json:"freshness"`
	ExpiresAt   *time.Time                `json:"expires_at,omitempty"`
	Events      []telemetry.ActivityEvent `json:"events"`
	Completed   []telemetry.Completed     `json:"completed"`
}

func boardItems(snapshot telemetry.Snapshot, projectID string, state string) []BoardItem {
	rows := make(map[string]BoardItem)
	add := func(issue telemetry.Issue) {
		key := issue.ProjectID + "\x00" + issue.ID
		rows[key] = BoardItem{ProjectID: issue.ProjectID, IssueID: issue.ID, Identifier: issue.Identifier, Title: issue.Title, State: issue.State, Priority: issue.Priority, PriorityName: issue.PriorityName}
	}
	for _, issue := range snapshot.BoardIssues {
		add(issue)
	}
	for _, issue := range snapshot.Pipeline {
		add(issue)
	}
	for _, running := range snapshot.Running {
		add(running.Issue)
		key := running.ProjectID + "\x00" + running.ID
		row := rows[key]
		row.Active = true
		row.Attempt = running.Attempt
		row.WorkAttemptID = running.WorkAttemptID
		row.DetentSessionID = running.DetentSessionID
		row.ProviderSessionID = running.SessionID
		rows[key] = row
	}
	for _, queued := range snapshot.Queue {
		add(queued.Issue)
	}
	for _, blocked := range snapshot.Blocked {
		issue := blocked.Issue
		issue.State = "Blocked"
		add(issue)
		key := blocked.ProjectID + "\x00" + blocked.ID
		row := rows[key]
		row.BlockedReason = blocked.Error
		row.BlockedSource = string(blocked.Source)
		rows[key] = row
	}
	for _, completed := range snapshot.Completed {
		add(completed.Issue)
		key := completed.ProjectID + "\x00" + completed.ID
		row := rows[key]
		completedAt := completed.CompletedAt
		row.CompletedAt = &completedAt
		rows[key] = row
	}
	out := make([]BoardItem, 0, len(rows))
	for _, row := range rows {
		if projectID != "" && !strings.EqualFold(row.ProjectID, strings.TrimSpace(projectID)) {
			continue
		}
		if state != "" && !strings.EqualFold(row.State, strings.TrimSpace(state)) {
			continue
		}
		out = append(out, row)
	}
	slices.SortFunc(out, func(left BoardItem, right BoardItem) int {
		return strings.Compare(left.Identifier, right.Identifier)
	})
	return out
}

// DecodeArguments applies the catalog's bounded strict decoder to shared commands.
func DecodeArguments(raw json.RawMessage, target any) error { return decodeArguments(raw, target) }
