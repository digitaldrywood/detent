package reports

import (
	"context"
	"net/url"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/requestmetrics"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

const MaxAnalyticsPopulation = 1000

type NativeAnalyticsBucket struct {
	LaneResidence               operatortool.AnalyticsResidenceSummary `json:"lane_residence"`
	ProviderCapacityWaitSeconds float64                                `json:"provider_capacity_wait_seconds"`
	From                        time.Time                              `json:"from"`
	To                          time.Time                              `json:"to"`
	Shipped                     int                                    `json:"shipped"`
	Sessions                    int                                    `json:"sessions"`
	Tokens                      int64                                  `json:"tokens"`
	Cost                        float64                                `json:"cost_usd"`
	Activity                    nativeAnalyticsActivityTiming          `json:"activity"`
	QueueSeconds                float64                                `json:"queue_seconds"`
}

type NativeAnalyticsLanding struct {
	ChangeID   string                   `json:"change_id"`
	WorkItemID tracker.NativeWorkItemID `json:"work_item_id"`
	Landing    tracker.ChangeLanding    `json:"landing"`
}

type NativeAnalyticsPhase struct {
	Name           string  `json:"name"`
	Count          int     `json:"count"`
	Seconds        float64 `json:"seconds"`
	AverageSeconds float64 `json:"average_seconds"`
}

type NativeAnalyticsAttempt struct {
	Pipeline            []gate.PipelineTiming `json:"pipeline,omitempty"`
	PipelineDropped     int                   `json:"pipeline_dropped,omitempty"`
	ActivityRaw         string                `json:"-"`
	publicActivity      bool
	UsageRowsObserved   int                              `json:"usage_rows_observed"`
	Identity            agentidentity.Identity           `json:"identity,omitzero"`
	Landing             *tracker.NativeLandingReceipt    `json:"landing,omitempty"`
	AttemptID           string                           `json:"attempt_id"`
	WorkItemID          string                           `json:"work_item_id"`
	Status              string                           `json:"status"`
	StartedAt           time.Time                        `json:"started_at"`
	ObservedAt          time.Time                        `json:"observed_at"`
	Phases              []tracker.NativePhase            `json:"phases"`
	Activity            *workflowmetrics.ActivityProfile `json:"activity,omitempty"`
	ActivityUnavailable string                           `json:"activity_unavailable,omitempty"`
}

type NativeAnalyticsSkip struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

type nativeAnalyticsOutcome struct {
	Shipped            int      `json:"shipped"`
	CostPerShipped     *float64 `json:"cost_per_shipped_usd,omitempty"`
	TokensPerShipped   *float64 `json:"tokens_per_shipped,omitempty"`
	PopulationObserved int      `json:"population_observed"`
	PopulationTotal    *int     `json:"population_total,omitempty"`
	Clipped            bool     `json:"clipped"`
}

type NativeAnalyticsProject struct {
	PopulationCursors    map[string]string                             `json:"population_cursors,omitempty"`
	PipelineTimings      []gate.PipelineTiming                         `json:"-"`
	AllAttempts          []NativeAnalyticsAttempt                      `json:"-"`
	AllLandings          []NativeAnalyticsLanding                      `json:"-"`
	Failures             []NativeFailure                               `json:"-"`
	Pipeline             PipelineReport                                `json:"pipeline"`
	PipelineReceipts     operatortool.ReadPage[gate.PipelineTiming]    `json:"pipeline_receipts"`
	Quality              AnalyticsQuality                              `json:"quality"`
	FailureSignatures    operatortool.ReadPage[NativeFailureSignature] `json:"failure_signatures"`
	LaneResidence        operatortool.AnalyticsResidence               `json:"lane_residence"`
	ProviderCapacityWait NativeAnalyticsCapacityWait                   `json:"provider_capacity_wait"`
	ProjectID            string                                        `json:"project_id"`
	Source               string                                        `json:"source"`
	SourceAt             time.Time                                     `json:"source_at"`
	Window               operatortool.AnalyticsWindow                  `json:"window"`
	Digest               []NativeAnalyticsBucket                       `json:"digest"`
	Efficiency           []NativeAnalyticsPhase                        `json:"efficiency"`
	CostPerOutcome       nativeAnalyticsOutcome                        `json:"cost_per_outcome"`
	Landings             operatortool.ReadPage[NativeAnalyticsLanding] `json:"landings"`
	Attempts             operatortool.ReadPage[NativeAnalyticsAttempt] `json:"attempts"`
	Activity             nativeAnalyticsActivity                       `json:"activity"`
	QueueTime            operatortool.AnalyticsQueue                   `json:"queue_time"`
	SkipReasons          []NativeAnalyticsSkip                         `json:"skip_reasons"`
	PopulationLimit      int                                           `json:"population_limit"`
	AttemptsObserved     int                                           `json:"attempts_observed"`
	DecisionsObserved    int                                           `json:"decisions_observed"`
	UsageRowsObserved    int                                           `json:"usage_rows_observed"`
	Partial              bool                                          `json:"partial"`
	Unavailable          []string                                      `json:"unavailable"`
}

type NativeAnalyticsReport struct {
	Requests       *requestmetrics.Report       `json:"requests,omitempty"`
	RouteLatency   *requestmetrics.History      `json:"route_latency,omitempty"`
	OrganizationID string                       `json:"organization_id"`
	Projects       []NativeAnalyticsProject     `json:"projects"`
	ObservedAt     time.Time                    `json:"observed_at"`
	Window         operatortool.AnalyticsWindow `json:"window"`
	NextOffset     *int                         `json:"next_offset,omitempty"`
}

func NativeAnalyticsBucketIndex(at time.Time, w operatortool.AnalyticsWindow) int {
	if at.Before(w.From) || !at.Before(w.To) {
		return -1
	}
	return int(at.Sub(w.From) / w.Bucket)
}

type AnalyticsCohortKey struct{}

type AnalyticsCohortSelection struct {
	Runners     []string                          `json:"-"`
	Items       map[tracker.NativeWorkItemID]bool `json:"-"`
	Fingerprint string                            `json:"-"`
}

func AnalyticsCohortAllows(ctx context.Context, id tracker.NativeWorkItemID) bool {
	selection, ok := ctx.Value(AnalyticsCohortKey{}).(*AnalyticsCohortSelection)
	return !ok || selection == nil || selection.Items[id]
}

func BindAnalyticsCohort(params url.Values, ctx context.Context) {
	if selection, ok := ctx.Value(AnalyticsCohortKey{}).(*AnalyticsCohortSelection); ok && selection != nil {
		params.Set("cohort", selection.Fingerprint)
	}
}

func AnalyticsCohortRunners(ctx context.Context) []string {
	selection, ok := ctx.Value(AnalyticsCohortKey{}).(*AnalyticsCohortSelection)
	if !ok || selection == nil {
		return nil
	}
	return selection.Runners
}

func AnalyticsCohortRunnerAllows(ctx context.Context, runner string) bool {
	selected := AnalyticsCohortRunners(ctx)
	return len(selected) == 0 || slices.Contains(selected, runner)
}
