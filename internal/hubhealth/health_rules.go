package hubhealth

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
	Interval            = time.Minute
	InstanceWindow      = 5 * time.Minute
	healthFlowWindow    = 30 * time.Minute
	ReopenWindow        = time.Hour
	HistoryWindow       = 24 * time.Hour
	ProjectLimit        = 100
	RunnerLimit         = 1000
	ReadLimit           = 10000
	healthEvidenceLimit = 20
)

type Subject struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type Evidence struct {
	Productivity        *healthProductivityEvidence `json:"productivity,omitempty"`
	ScheduledValidation *healthScheduledEvidence    `json:"scheduled_validation,omitempty"`
	EventIDs            []string                    `json:"event_ids,omitempty"`
	AttemptIDs          []string                    `json:"attempt_ids,omitempty"`
	WorkItemIDs         []string                    `json:"work_item_ids,omitempty"`
	Counts              map[string]int              `json:"counts,omitempty"`
	Values              map[string]float64          `json:"values,omitempty"`
	Queues              map[string]QueueEvidence    `json:"queues,omitempty"`
	Signatures          []string                    `json:"signatures,omitempty"`
	Reason              string                      `json:"reason,omitempty"`
}

type healthScheduledEvidence struct {
	LastReportAt   time.Time  `json:"last_report_at"`
	LastRunURL     string     `json:"last_run_url"`
	LastGreenAt    *time.Time `json:"last_green_at,omitempty"`
	CadenceSeconds int        `json:"cadence_seconds"`
	WindowSeconds  int        `json:"window_seconds"`
}

type ScheduledValidation struct {
	FirstReport time.Time
	LastReport  time.Time
	LastGreen   time.Time
	LastRunURL  string
	Cadence     time.Duration
	Failures    int
	LastFailed  bool
}

type SlackFailure struct {
	Code       string    `json:"code"`
	StatusCode int       `json:"status_code"`
	At         time.Time `json:"at"`
}

type Finding struct {
	ConditionSince   time.Time      `json:"-"`
	SlackUnavailable *SlackFailure  `json:"slack_unavailable,omitempty"`
	ID               string         `json:"id"`
	Fingerprint      string         `json:"fingerprint"`
	Signal           string         `json:"signal"`
	Class            string         `json:"class"`
	Subject          Subject        `json:"subject"`
	Projects         []string       `json:"-"`
	OpenedAt         time.Time      `json:"opened_at"`
	LastSeenAt       time.Time      `json:"last_seen_at"`
	ResolvedAt       *time.Time     `json:"resolved_at"`
	Severity         string         `json:"severity"`
	Summary          string         `json:"summary"`
	NextAction       string         `json:"next_action"`
	Evidence         Evidence       `json:"evidence"`
	EmailUnavailable bool           `json:"email_unavailable,omitempty"`
	FiledBy          string         `json:"filed_by,omitempty"`
	Issues           []FindingIssue `json:"issues,omitempty"`
}

type FindingIssue struct {
	ProjectID  tracker.ProjectID        `json:"project_id"`
	WorkItemID tracker.NativeWorkItemID `json:"work_item_id"`
}

func ScopeEvidence(evidence Evidence, project tracker.ProjectID) Evidence {
	if len(evidence.Queues) == 0 {
		return evidence
	}
	queue, ok := evidence.Queues[string(project)]
	evidence.Queues = map[string]QueueEvidence{}
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

type Runner struct {
	ID        string
	Projects  []string
	Heartbeat time.Time
	CreatedAt time.Time
	Leases    int
	FreeSlots int
}

type Project struct {
	Scheduled       ScheduledValidation
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
	DependencyGated int
	HeldCount       int
	HeldBlockers    []string
	HoldSince       time.Time
}

type Wait struct {
	Reason  string
	Item    string
	Project string
	Since   time.Time
	Attempt string
	Event   string
	Active  bool
}

type Snapshot struct {
	Runners             []Runner
	Projects            []Project
	Waits               []Wait
	Rates               []RateProject
	BaselineUnavailable []BaselineUnavailable
}

func NewFinding(signal, class, kind, id, summary, action string, projects []string, evidence Evidence) Finding {
	sum := sha256.Sum256([]byte(signal + "\x00" + kind + "\x00" + id))
	evidence.Signatures = slices.Clone(evidence.Signatures[:min(len(evidence.Signatures), healthEvidenceLimit)])
	evidence.EventIDs = slices.Clone(evidence.EventIDs[:min(len(evidence.EventIDs), healthEvidenceLimit)])
	evidence.AttemptIDs = slices.Clone(evidence.AttemptIDs[:min(len(evidence.AttemptIDs), healthEvidenceLimit)])
	evidence.WorkItemIDs = slices.Clone(evidence.WorkItemIDs[:min(len(evidence.WorkItemIDs), healthEvidenceLimit)])
	return Finding{Fingerprint: hex.EncodeToString(sum[:]), Signal: signal, Class: class, Subject: Subject{kind, id}, Projects: slices.Clone(projects), Severity: "attention", Summary: summary, NextAction: action, Evidence: evidence}
}

func HeartbeatFinding(now time.Time, r Runner) (Finding, bool) {
	baseline := r.Heartbeat
	if baseline.IsZero() {
		baseline = r.CreatedAt
	}
	active := (r.Leases > 0 || len(r.Projects) > 0) && !baseline.IsZero() && now.Sub(baseline) >= InstanceWindow
	return NewFinding("runner_heartbeat_gap", "instance", "runner", r.ID, "Enrolled runner has not sent a heartbeat for five minutes.", "The runner operator checks the runner process and its Hub connection.", r.Projects, Evidence{Counts: map[string]int{"leases": r.Leases}}), active
}

func DeadManFinding(now time.Time, p Project) (Finding, bool) {
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
	evidence := Evidence{Counts: map[string]int{"candidates": p.Candidates, "free_slots": p.FreeSlots, "landing_stalled": 0, "scheduled_silent": 0, "scheduled_red": 0}}
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
	return NewFinding("dead_man", "flow", "project", p.ID, "Delivery stalled. "+strings.Join(reasons, " "), action, []string{p.ID}, evidence), landing || silent || red
}

func dependencyHoldFinding(now time.Time, p Project) (Finding, bool) {
	active := p.Candidates == 0 && p.DependencyGated > 0 && p.HeldCount > 0 && p.FreeSlots > 0 && !p.HoldSince.IsZero() && now.Sub(p.HoldSince) >= healthFlowWindow && (p.LastLanding.IsZero() || now.Sub(p.LastLanding) >= healthFlowWindow)
	evidence := Evidence{Counts: map[string]int{"dependency_gated": p.DependencyGated, "held_blockers": p.HeldCount, "free_slots": p.FreeSlots}, WorkItemIDs: p.HeldBlockers}
	summary := fmt.Sprintf("Delivery stalled. %d dispatchable items wait on dependencies held in non-dispatchable lanes while runner slots are free.", p.DependencyGated)
	return NewFinding("dependency_hold", "flow", "project", p.ID, summary, "The project operator moves, finishes or removes the held blocking items listed in the evidence.", []string{p.ID}, evidence), active
}

func schedulerFinding(now time.Time, p Project) (Finding, bool) {
	active := !p.RefreshAt.IsZero() && (now.Sub(p.RefreshAt) > InstanceWindow || p.RefreshDuration > InstanceWindow)
	return NewFinding("scheduler_loop_behind", "instance", "project", p.ID, "The project scheduler refresh is more than five minutes behind.", "The runner operator checks project refresh timing and errors.", []string{p.ID}, Evidence{Counts: map[string]int{"refresh_seconds": int(p.RefreshDuration / time.Second)}}), active
}

func RefusalFinding(p Project) (Finding, bool) {
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
	return NewFinding("every_candidate_refused", "instance", "project", p.ID, summary, action, []string{p.ID}, Evidence{EventIDs: p.RefusalEvents, Counts: map[string]int{"candidates": p.Candidates, "claims": p.Claims}, Reason: reason}), active
}

func HumanWaitFinding(now time.Time, w Wait) (Finding, bool) {
	evidence := Evidence{}
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
	finding := NewFinding("unanswered_human_wait", "human", kind, id, summary, action, []string{w.Project}, evidence)
	finding.ConditionSince = w.Since
	return finding, w.Active && (w.Reason != "" || !w.Since.IsZero() && now.Sub(w.Since) > healthFlowWindow)
}

func Evaluate(now time.Time, snapshot Snapshot) []Finding {
	findings := []Finding{}
	add := func(f Finding, active bool) {
		if active {
			findings = append(findings, f)
		}
	}
	for _, r := range snapshot.Runners {
		add(HeartbeatFinding(now, r))
	}
	for _, p := range snapshot.Projects {
		add(DeadManFinding(now, p))
		add(dependencyHoldFinding(now, p))
		add(schedulerFinding(now, p))
		add(RefusalFinding(p))
	}
	for _, w := range snapshot.Waits {
		add(HumanWaitFinding(now, w))
	}
	findings = append(findings, evaluateHealthRates(now, snapshot.Rates)...)
	slices.SortFunc(findings, func(a, b Finding) int {
		if a.Fingerprint < b.Fingerprint {
			return -1
		}
		if a.Fingerprint > b.Fingerprint {
			return 1
		}
		return 0
	})
	return slices.CompactFunc(findings, func(a, b Finding) bool { return a.Fingerprint == b.Fingerprint })
}
