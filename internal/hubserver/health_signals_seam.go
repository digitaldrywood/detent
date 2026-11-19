package hubserver

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
)

type healthSubject = healthsignals.Subject
type healthEvidence = healthsignals.Evidence
type healthScheduledValidation = healthsignals.ScheduledValidation
type healthSlackFailure = healthsignals.SlackFailure
type healthFinding = healthsignals.Finding
type healthFindingIssue = healthsignals.FindingIssue
type healthRunner = healthsignals.Runner
type healthProject = healthsignals.Project
type healthWait = healthsignals.Wait
type healthSnapshot = healthsignals.Snapshot
type healthRateAttempt = healthsignals.RateAttempt
type healthItemCost = healthsignals.ItemCost
type healthRateProject = healthsignals.RateProject
type healthQueueEvidence = healthsignals.QueueEvidence
type healthBaselineUnavailable = healthsignals.BaselineUnavailable
type healthRateBaseline = healthsignals.RateBaseline
type healthProductivityEvidence = healthsignals.ProductivityEvidence

var errHealthReadLimit = healthsignals.ErrReadLimit

const (
	healthInterval        = healthsignals.Interval
	healthInstanceWindow  = healthsignals.InstanceWindow
	healthFlowWindow      = healthsignals.FlowWindow
	healthReopenWindow    = healthsignals.ReopenWindow
	healthHistoryWindow   = healthsignals.HistoryWindow
	healthProjectLimit    = healthsignals.ProjectLimit
	healthRunnerLimit     = healthsignals.RunnerLimit
	healthReadLimit       = healthsignals.ReadLimit
	healthEvidenceLimit   = healthsignals.EvidenceLimit
	healthBaselineWindow  = healthsignals.BaselineWindow
	healthLongWindow      = healthsignals.LongWindow
	healthBaselineSamples = healthsignals.BaselineSamples
)

func newHealthFinding(signal, class, kind, id, summary, action string, projects []string, evidence healthEvidence) healthFinding {
	return healthsignals.NewFinding(signal, class, kind, id, summary, action, projects, evidence)
}
func heartbeatFinding(now time.Time, r healthRunner) (healthFinding, bool) {
	return healthsignals.HeartbeatFinding(now, r)
}
func deadManFinding(now time.Time, p healthProject) (healthFinding, bool) {
	return healthsignals.DeadManFinding(now, p)
}
func dependencyHoldFinding(now time.Time, p healthProject) (healthFinding, bool) {
	return healthsignals.DependencyHoldFinding(now, p)
}
func schedulerFinding(now time.Time, p healthProject) (healthFinding, bool) {
	return healthsignals.SchedulerFinding(now, p)
}
func refusalFinding(p healthProject) (healthFinding, bool) { return healthsignals.RefusalFinding(p) }
func humanWaitFinding(now time.Time, w healthWait) (healthFinding, bool) {
	return healthsignals.HumanWaitFinding(now, w)
}
func evaluateHealth(now time.Time, snapshot healthSnapshot) []healthFinding {
	return healthsignals.Evaluate(now, snapshot)
}
func buildHealthRateBaseline(now time.Time, p healthRateProject) healthRateBaseline {
	return healthsignals.BuildRateBaseline(now, p)
}
func healthBaselineGaps(p healthRateProject, b healthRateBaseline) []healthBaselineUnavailable {
	return healthsignals.BaselineGaps(p, b)
}
func evaluateHealthRates(now time.Time, p []healthRateProject) []healthFinding {
	return healthsignals.EvaluateRates(now, p)
}
func healthRecoveryReason(text string) string {
	return healthsignals.New(healthsignals.Deps{NormalizeFailureSignature: normalizeFailureSignature}).RecoveryReason(text)
}
func healthResidenceTimeline(t analyticsResidenceTimeline) healthsignals.ResidenceTimeline {
	out := healthsignals.ResidenceTimeline{Issue: healthsignals.ResidenceIssue{ID: t.Issue.Id, Partial: t.Issue.Partial}, State: t.State, Entered: t.Entered, Done: t.Done}
	if t.Spans != nil {
		out.Spans = make([]healthsignals.ResidenceSpan, len(t.Spans))
		for i, s := range t.Spans {
			out.Spans[i] = healthsignals.ResidenceSpan{Lane: s.Lane, From: s.From, To: s.To}
		}
	}
	return out
}
