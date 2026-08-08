package operatortool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
}

type Executor struct {
	snapshots SnapshotSource
	explainer Explainer
}

func NewExecutor(deps Dependencies) *Executor {
	return &Executor{snapshots: deps.Snapshots, explainer: deps.Explainer}
}

func (e *Executor) Execute(ctx context.Context, call Call) (Result, error) {
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

func (e *Executor) boardState(ctx context.Context, raw json.RawMessage) (Result, error) {
	var request struct {
		ProjectID string `json:"project_id"`
		State     string `json:"state"`
		Limit     int    `json:"limit"`
	}
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
		BackendOutages:     boundedClone(snapshot.BackendOutages, MaxItemLimit),
		FailureBreakers:    boundedClone(snapshot.FailureBreakers, MaxItemLimit),
		DispatchRecoveries: boundedClone(snapshot.DispatchRecoveries, MaxItemLimit),
	})
}

func (e *Executor) telemetryUsage(ctx context.Context, raw json.RawMessage) (Result, error) {
	var request struct {
		ProjectID string `json:"project_id"`
	}
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
	var request struct {
		ProjectID string `json:"project_id"`
		Limit     int    `json:"limit"`
	}
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
	var request struct {
		ProjectID string `json:"project_id"`
		Reference string `json:"reference"`
	}
	if err := decodeArguments(raw, &request); err != nil {
		return Result{}, err
	}
	request.ProjectID = strings.TrimSpace(request.ProjectID)
	request.Reference = strings.TrimSpace(request.Reference)
	if request.ProjectID == "" || request.Reference == "" {
		return Result{}, fmt.Errorf("%w: project_id and reference are required", ErrInvalidArguments)
	}
	if e.explainer == nil {
		return Result{}, errors.New("issue explanation is unavailable")
	}
	result, err := e.explainer.Explain(ctx, explain.Query{ProjectID: request.ProjectID, Reference: request.Reference})
	if err != nil {
		return Result{}, err
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
	return snapshot, nil
}

func decodeArguments(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	if len(raw) > MaxArgumentBytes {
		return fmt.Errorf("%w: payload exceeds %d bytes", ErrInvalidArguments, MaxArgumentBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	return nil
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
		return Result{}, fmt.Errorf("operator tool result exceeds %d bytes", MaxResultBytes)
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
	if rateLimits.GraphQLCost != nil {
		graphql := *rateLimits.GraphQLCost
		graphql.Contributors = boundedClone(graphql.Contributors, MaxItemLimit)
		result.GraphQLCost = &graphql
	}
	if rateLimits.RESTUsage != nil {
		rest := *rateLimits.RESTUsage
		rest.Contributors = boundedClone(rest.Contributors, MaxItemLimit)
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
