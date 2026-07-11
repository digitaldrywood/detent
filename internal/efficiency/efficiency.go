package efficiency

import (
	"context"
	"time"
)

type Thresholds struct {
	TokensMultiple   float64
	SessionsMultiple float64
	DwellMultiple    float64
}

type Completion struct {
	ProjectID   string
	IssueID     string
	Identifier  string
	IssueURL    string
	PRNumber    *int64
	CompletedAt time.Time
	Thresholds  Thresholds
}

type Receipt struct {
	ProjectID             string
	IssueID               string
	Identifier            string
	IssueURL              string
	PRNumber              *int64
	Sessions              int64
	Attempts              int64
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
	EstimatedCostUSD      float64
	FirstDispatchedAt     time.Time
	CompletedAt           time.Time
	WallSeconds           int64
	WorkingSeconds        int64
	GateWaitSeconds       int64
	MergeTrainSeconds     int64
	ParkedSeconds         int64
	Redispatches          int64
	BreakerTrips          int64
	CIReruns              int64
	TokensBaseline        float64
	SessionsBaseline      float64
	DwellBaselineSeconds  float64
	TokensAnomaly         bool
	SessionsAnomaly       bool
	DwellAnomaly          bool
}

func (r Receipt) CacheShare() float64 {
	if r.InputTokens <= 0 {
		return 0
	}
	return float64(r.CachedInputTokens) / float64(r.InputTokens)
}

func (r Receipt) FreshInputTokens() int64 {
	fresh := r.InputTokens - r.CachedInputTokens
	if fresh < 0 {
		return 0
	}
	return fresh
}

func (r Receipt) Anomalous() bool {
	return r.TokensAnomaly || r.SessionsAnomaly || r.DwellAnomaly
}

type Query struct {
	ProjectID string
	From      time.Time
	To        time.Time
	Limit     int
}

type Percentiles struct {
	P50 float64
	P90 float64
}

type Dwell struct {
	WorkingSeconds    int64
	GateWaitSeconds   int64
	MergeTrainSeconds int64
	ParkedSeconds     int64
}

type RollupWindow struct {
	Issues                int64
	TokensPerIssue        Percentiles
	CostPerIssueUSD       Percentiles
	CacheShare            float64
	SessionsPerIssue      float64
	FirstAttemptMergeRate float64
	Dwell                 Dwell
	Anomalies             int64
}

type TrendPoint struct {
	Day        string
	CacheShare float64
}

type Rollup struct {
	From       time.Time
	To         time.Time
	Current    RollupWindow
	Baseline   RollupWindow
	CacheTrend []TrendPoint
}

type Recorder interface {
	CompleteEfficiencyReceipt(context.Context, Completion) (Receipt, error)
}

type Reader interface {
	EfficiencyReceipt(context.Context, string, string, string) (Receipt, error)
	ListEfficiencyReceipts(context.Context, Query) ([]Receipt, error)
	EfficiencyRollup(context.Context, Query) (Rollup, error)
}
