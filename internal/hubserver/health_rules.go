package hubserver

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	healthInterval       = time.Minute
	healthInstanceWindow = 5 * time.Minute
	healthFlowWindow     = 30 * time.Minute
	healthReopenWindow   = time.Hour
	healthHistoryWindow  = 24 * time.Hour
	healthProjectLimit   = 100
	healthRunnerLimit    = 1000
	healthReadLimit      = 10000
	healthEvidenceLimit  = 20
)

type healthSubject struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type healthEvidence struct {
	ScheduledValidation *healthScheduledEvidence       `json:"scheduled_validation,omitempty"`
	EventIDs            []string                       `json:"event_ids,omitempty"`
	AttemptIDs          []string                       `json:"attempt_ids,omitempty"`
	Counts              map[string]int                 `json:"counts,omitempty"`
	Values              map[string]float64             `json:"values,omitempty"`
	Queues              map[string]healthQueueEvidence `json:"queues,omitempty"`
	Signatures          []string                       `json:"signatures,omitempty"`
	Reason              string                         `json:"reason,omitempty"`
}

type healthScheduledEvidence struct {
	LastReportAt   time.Time  `json:"last_report_at"`
	LastRunURL     string     `json:"last_run_url"`
	LastGreenAt    *time.Time `json:"last_green_at,omitempty"`
	CadenceSeconds int        `json:"cadence_seconds"`
	WindowSeconds  int        `json:"window_seconds"`
}

type healthScheduledValidation struct {
	FirstReport time.Time
	LastReport  time.Time
	LastGreen   time.Time
	LastRunURL  string
	Cadence     time.Duration
	Failures    int
	LastFailed  bool
}

type healthSlackFailure struct {
	Code       string    `json:"code"`
	StatusCode int       `json:"status_code"`
	At         time.Time `json:"at"`
}

type healthFinding struct {
	SlackUnavailable *healthSlackFailure  `json:"slack_unavailable,omitempty"`
	ID               string               `json:"id"`
	Fingerprint      string               `json:"fingerprint"`
	Signal           string               `json:"signal"`
	Class            string               `json:"class"`
	Subject          healthSubject        `json:"subject"`
	Projects         []string             `json:"-"`
	OpenedAt         time.Time            `json:"opened_at"`
	LastSeenAt       time.Time            `json:"last_seen_at"`
	ResolvedAt       *time.Time           `json:"resolved_at"`
	Severity         string               `json:"severity"`
	Summary          string               `json:"summary"`
	NextAction       string               `json:"next_action"`
	Evidence         healthEvidence       `json:"evidence"`
	EmailUnavailable bool                 `json:"email_unavailable,omitempty"`
	FiledBy          string               `json:"filed_by,omitempty"`
	Issues           []healthFindingIssue `json:"issues,omitempty"`
}

type healthFindingIssue struct {
	ProjectID  tracker.ProjectID        `json:"project_id"`
	WorkItemID tracker.NativeWorkItemID `json:"work_item_id"`
}

func scopeHealthEvidence(evidence healthEvidence, project tracker.ProjectID) healthEvidence {
	if len(evidence.Queues) == 0 {
		return evidence
	}
	queue, ok := evidence.Queues[string(project)]
	evidence.Queues = map[string]healthQueueEvidence{}
	evidence.Counts = maps.Clone(evidence.Counts)
	if ok {
		evidence.Queues[string(project)] = queue
		if evidence.Counts == nil {
			evidence.Counts = map[string]int{}
		}
		evidence.Counts["queue_depth"] = queue.QueueDepth
	}
	return evidence
}

type healthRunner struct {
	ID        string
	Projects  []string
	Heartbeat time.Time
	CreatedAt time.Time
	Leases    int
	FreeSlots int
}

type healthProject struct {
	Scheduled       healthScheduledValidation
	ID              string
	Candidates      int
	CandidateIDs    map[string]bool
	CandidateSince  time.Time
	LastLanding     time.Time
	FreeSlots       int
	RefreshAt       time.Time
	RefreshDuration time.Duration
	RefusalReasons  []string
	RefusalEvents   []string
	Claims          int
}

type healthWait struct {
	Reason  string
	Item    string
	Project string
	Since   time.Time
	Attempt string
	Event   string
	Active  bool
}

type healthLimit struct {
	Item    string
	Project string
	Event   string
	Active  bool
}

type healthSnapshot struct {
	Runners             []healthRunner
	Projects            []healthProject
	Waits               []healthWait
	Rates               []healthRateProject
	BaselineUnavailable []healthBaselineUnavailable
	Limits              []healthLimit
}

func newHealthFinding(signal, class, kind, id, summary, action string, projects []string, evidence healthEvidence) healthFinding {
	sum := sha256.Sum256([]byte(signal + "\x00" + kind + "\x00" + id))
	evidence.Signatures = slices.Clone(evidence.Signatures[:min(len(evidence.Signatures), healthEvidenceLimit)])
	evidence.EventIDs = slices.Clone(evidence.EventIDs[:min(len(evidence.EventIDs), healthEvidenceLimit)])
	evidence.AttemptIDs = slices.Clone(evidence.AttemptIDs[:min(len(evidence.AttemptIDs), healthEvidenceLimit)])
	return healthFinding{Fingerprint: hex.EncodeToString(sum[:]), Signal: signal, Class: class, Subject: healthSubject{kind, id}, Projects: slices.Clone(projects), Severity: "attention", Summary: summary, NextAction: action, Evidence: evidence}
}

func heartbeatFinding(now time.Time, r healthRunner) (healthFinding, bool) {
	baseline := r.Heartbeat
	if baseline.IsZero() {
		baseline = r.CreatedAt
	}
	active := (r.Leases > 0 || len(r.Projects) > 0) && !baseline.IsZero() && now.Sub(baseline) >= healthInstanceWindow
	return newHealthFinding("runner_heartbeat_gap", "instance", "runner", r.ID, "Enrolled runner has not sent a heartbeat for five minutes.", "The runner operator checks the runner process and its Hub connection.", r.Projects, healthEvidence{Counts: map[string]int{"leases": r.Leases}}), active
}

func deadManFinding(now time.Time, p healthProject) (healthFinding, bool) {
	baseline := p.LastLanding
	if baseline.IsZero() {
		baseline = p.CandidateSince
	}
	landing := p.Candidates > 0 && p.FreeSlots > 0 && !baseline.IsZero() && now.Sub(baseline) >= healthFlowWindow
	scheduled := p.Scheduled
	window := max(6*time.Hour, 2*scheduled.Cadence)
	silent := !scheduled.LastReport.IsZero() && now.Sub(scheduled.LastReport) >= window
	greenBaseline := scheduled.LastGreen
	if greenBaseline.IsZero() {
		greenBaseline = scheduled.FirstReport
	}
	red := !silent && scheduled.LastFailed && scheduled.Failures >= 2 && !greenBaseline.IsZero() && now.Sub(greenBaseline) >= 24*time.Hour
	evidence := healthEvidence{Counts: map[string]int{"candidates": p.Candidates, "free_slots": p.FreeSlots, "landing_stalled": 0, "scheduled_silent": 0, "scheduled_red": 0}}
	reasons := []string{}
	if landing {
		evidence.Counts["landing_stalled"] = 1
		reasons = append(reasons, "No work has landed for thirty minutes despite candidates and a free slot.")
	}
	if silent || red {
		evidence.ScheduledValidation = &healthScheduledEvidence{LastReportAt: scheduled.LastReport, LastRunURL: scheduled.LastRunURL, CadenceSeconds: int(scheduled.Cadence / time.Second), WindowSeconds: int(window / time.Second)}
		if !scheduled.LastGreen.IsZero() {
			evidence.ScheduledValidation.LastGreenAt = &scheduled.LastGreen
		}
	}
	if silent {
		evidence.Counts["scheduled_silent"] = 1
		reasons = append(reasons, fmt.Sprintf("Scheduled validation is silent; last report at %s (%s).", scheduled.LastReport.UTC().Format(time.RFC3339), scheduled.LastRunURL))
	}
	if red {
		evidence.Counts["scheduled_red"] = 1
		lastGreen := "never reported"
		if !scheduled.LastGreen.IsZero() {
			lastGreen = scheduled.LastGreen.UTC().Format(time.RFC3339)
		}
		reasons = append(reasons, "Scheduled validation has stayed red for at least twenty-four hours; last green: "+lastGreen+".")
	}
	action := "The project operator checks scheduler admission and landing evidence."
	if silent || red {
		action = "The project operator checks the scheduled validation host and linked run evidence and restores reporting or fixes the failing validation."
	}
	return newHealthFinding("dead_man", "flow", "project", p.ID, "Delivery stalled. "+strings.Join(reasons, " "), action, []string{p.ID}, evidence), landing || silent || red
}

func schedulerFinding(now time.Time, p healthProject) (healthFinding, bool) {
	active := !p.RefreshAt.IsZero() && (now.Sub(p.RefreshAt) > healthInstanceWindow || p.RefreshDuration > healthInstanceWindow)
	return newHealthFinding("scheduler_loop_behind", "instance", "project", p.ID, "The project scheduler refresh is more than five minutes behind.", "The runner operator checks project refresh timing and errors.", []string{p.ID}, healthEvidence{Counts: map[string]int{"refresh_seconds": int(p.RefreshDuration / time.Second)}}), active
}

func refusalFinding(p healthProject) (healthFinding, bool) {
	active := p.Candidates > 0 && p.Claims == 0 && len(p.RefusalReasons) == p.Candidates
	reason := ""
	for _, value := range p.RefusalReasons {
		if value == "" || value == "runner_capacity_full" || value == "capacity_full" || reason != "" && value != reason {
			active = false
		}
		reason = value
	}
	summary := "Candidates cannot start work because runners refused every claim."
	action := "The runner operator checks the claim refusal in the runner's logs."
	switch reason {
	case "runner_heartbeat_stale":
		summary = "Candidates cannot start work because their runners are offline."
		action = "The runner operator checks the runner process and its Hub connection."
	case "runner_inactive":
		summary = "Candidates cannot start work because their runners are paused or finishing current work."
		action = "The runner operator reviews routing in Settings → Providers & runners."
	}
	return newHealthFinding("every_candidate_refused", "instance", "project", p.ID, summary, action, []string{p.ID}, healthEvidence{EventIDs: p.RefusalEvents, Counts: map[string]int{"candidates": p.Candidates, "claims": p.Claims}, Reason: reason}), active
}

func humanWaitFinding(now time.Time, w healthWait) (healthFinding, bool) {
	evidence := healthEvidence{}
	if w.Attempt != "" {
		evidence.AttemptIDs = []string{w.Attempt}
	}
	if w.Event != "" {
		evidence.EventIDs = []string{w.Event}
	}
	kind, id := "work_item", w.Item
	if id == "" {
		kind, id = "project", w.Project
	}
	summary := "A human question or permission wait has been unanswered for thirty minutes."
	action := "The responsible human answers the question or performs the recorded permission action."
	if w.Reason != "" {
		summary = "The issue cannot be dispatched until its required contract is completed and confirmed."
		action = w.Reason
		evidence.Reason = "issue_contract_unsatisfied"
	}
	return newHealthFinding("unanswered_human_wait", "human", kind, id, summary, action, []string{w.Project}, evidence), w.Active && (w.Reason != "" || !w.Since.IsZero() && now.Sub(w.Since) > healthFlowWindow)
}

func lifetimeFinding(l healthLimit) (healthFinding, bool) {
	return newHealthFinding("lifetime_limit_hit", "instance", "work_item", l.Item, "The work item has reached its lifetime execution limit.", "The runner operator reviews the linked work item and its lifetime accounting.", []string{l.Project}, healthEvidence{EventIDs: []string{l.Event}}), l.Active
}

func evaluateHealth(now time.Time, snapshot healthSnapshot) []healthFinding {
	findings := []healthFinding{}
	add := func(f healthFinding, active bool) {
		if active {
			findings = append(findings, f)
		}
	}
	for _, r := range snapshot.Runners {
		add(heartbeatFinding(now, r))
	}
	for _, p := range snapshot.Projects {
		add(deadManFinding(now, p))
		add(schedulerFinding(now, p))
		add(refusalFinding(p))
	}
	for _, w := range snapshot.Waits {
		add(humanWaitFinding(now, w))
	}
	for _, l := range snapshot.Limits {
		add(lifetimeFinding(l))
	}
	findings = append(findings, evaluateHealthRates(now, snapshot.Rates)...)
	slices.SortFunc(findings, func(a, b healthFinding) int {
		if a.Fingerprint < b.Fingerprint {
			return -1
		}
		if a.Fingerprint > b.Fingerprint {
			return 1
		}
		return 0
	})
	return slices.CompactFunc(findings, func(a, b healthFinding) bool { return a.Fingerprint == b.Fingerprint })
}
