package hubanalytics

import (
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type Bucket struct {
	LaneResidence               operatortool.AnalyticsResidenceSummary `json:"lane_residence"`
	ProviderCapacityWaitSeconds float64                                `json:"provider_capacity_wait_seconds"`
	From                        time.Time                              `json:"from"`
	To                          time.Time                              `json:"to"`
	Shipped                     int                                    `json:"shipped"`
	Sessions                    int                                    `json:"sessions"`
	Tokens                      int64                                  `json:"tokens"`
	Cost                        float64                                `json:"cost_usd"`
	Activity                    ActivityTiming                         `json:"activity"`
	QueueSeconds                float64                                `json:"queue_seconds"`
}

type Phase struct {
	Name           string  `json:"name"`
	Count          int     `json:"count"`
	Seconds        float64 `json:"seconds"`
	AverageSeconds float64 `json:"average_seconds"`
}

type Attempt struct {
	Pipeline            []gate.PipelineTiming            `json:"pipeline,omitempty"`
	PipelineDropped     int                              `json:"pipeline_dropped,omitempty"`
	ActivityRaw         string                           `json:"-"`
	PublicActivity      bool                             `json:"-"`
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

type Skip struct {
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

type Project struct {
	PopulationCursors    map[string]string                          `json:"population_cursors,omitempty"`
	PipelineTimings      []gate.PipelineTiming                      `json:"-"`
	AllAttempts          []Attempt                                  `json:"-"`
	AllLandings          []Landing                                  `json:"-"`
	Failures             []Failure                                  `json:"-"`
	Pipeline             PipelineReport                             `json:"pipeline"`
	PipelineReceipts     operatortool.ReadPage[gate.PipelineTiming] `json:"pipeline_receipts"`
	Quality              Quality                                    `json:"quality"`
	FailureSignatures    operatortool.ReadPage[FailureSignature]    `json:"failure_signatures"`
	LaneResidence        operatortool.AnalyticsResidence            `json:"lane_residence"`
	ProviderCapacityWait CapacityWait                               `json:"provider_capacity_wait"`
	ProjectID            string                                     `json:"project_id"`
	Source               string                                     `json:"source"`
	SourceAt             time.Time                                  `json:"source_at"`
	Window               operatortool.AnalyticsWindow               `json:"window"`
	Digest               []Bucket                                   `json:"digest"`
	Efficiency           []Phase                                    `json:"efficiency"`
	CostPerOutcome       nativeAnalyticsOutcome                     `json:"cost_per_outcome"`
	Landings             operatortool.ReadPage[Landing]             `json:"landings"`
	Attempts             operatortool.ReadPage[Attempt]             `json:"attempts"`
	Activity             nativeAnalyticsActivity                    `json:"activity"`
	QueueTime            operatortool.AnalyticsQueue                `json:"queue_time"`
	SkipReasons          []Skip                                     `json:"skip_reasons"`
	PopulationLimit      int                                        `json:"population_limit"`
	AttemptsObserved     int                                        `json:"attempts_observed"`
	DecisionsObserved    int                                        `json:"decisions_observed"`
	UsageRowsObserved    int                                        `json:"usage_rows_observed"`
	Partial              bool                                       `json:"partial"`
	Unavailable          []string                                   `json:"unavailable"`
}

const MaxPopulation = 1000
