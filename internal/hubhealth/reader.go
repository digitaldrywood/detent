package hubhealth

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/hubsql"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Scope struct {
	Reporter     bool
	Principal    string
	Organization tracker.OrganizationID
	Project      tracker.ProjectID
}
type ContractObservation struct {
	Issue              tracker.NativeIssue
	ReasonCode, Reason string
	Cacheable          bool
}
type ContractLoader func(context.Context, hubsql.Queryer, Scope, string) (ContractObservation, error)
type ReaderHooks struct {
	Candidates        func(context.Context, hubsql.Queryer, Scope, time.Time) ([]tracker.WorkItemID, error)
	Admissions        func(context.Context, hubsql.Queryer, tracker.OrganizationID, string, tracker.MachineID) (map[string]tracker.NativeAdmissionObservation, error)
	Repository        func(context.Context, hubsql.Queryer, string) (string, error)
	DecodeStorageJSON func(context.Context, []byte, any) error
	DecodeChangeJSON  func(context.Context, []byte, any) error
	ApplyAttemptData  func(*RateAttempt, tracker.NativeRunData)
	ColumnTime        func(time.Time) string
	LegacyReadLimit   int
	ResidenceHistory  func(context.Context, hubsql.Queryer, Scope, operatortool.AnalyticsWindow) (map[string]tracker.NativeState, []hubanalytics.ResidenceIssue, bool, error)
	Contract          ContractLoader
}
type SnapshotReader struct{ hooks ReaderHooks }

func NewSnapshotReader(hooks ReaderHooks) *SnapshotReader { return &SnapshotReader{hooks: hooks} }

type scheduledValidationRecord struct {
	gate.ScheduledValidationReport
	RecordedAt time.Time `json:"recorded_at"`
}

func scheduledRepairOriginMatches(source string, evidence gate.ScheduledEvidence) bool {
	runURL := "https://github.com/" + evidence.Repository + "/actions/runs/" + evidence.RunID
	return source == runURL || source == runURL+"/attempts/"+evidence.RunAttempt
}
func placeholders(count int) string { return strings.TrimSuffix(strings.Repeat("?,", count), ",") }
