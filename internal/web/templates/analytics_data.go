package templates

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

// analyticsView renders scheduler internals as ONE dense log — no cards,
// no filled badges. This is a debugging surface; exceptions surface on
// the Board, not here.
type analyticsView struct {
	Summary  string
	Kind     string
	Rows     []analyticsRow
	Filtered bool
}

type analyticsRow struct {
	ID       string
	At       time.Time
	Time     string
	Event    string
	Project  string
	Ref      string
	Kind     primitives.Kind
	Decision string
	Detail   string
}

const (
	analyticsKindAttempts = "attempts"
	analyticsKindActivity = "activity"
)

func analyticsViewFromDashboard(data DashboardData) analyticsView {
	kind := analyticsKindFilter(data.AnalyticsKind)
	rows := make([]analyticsRow, 0, len(data.Snapshot.WorkAttempts)+len(data.Snapshot.Events))
	if kind == "" || kind == analyticsKindAttempts {
		rows = append(rows, analyticsAttemptRows(data.Snapshot)...)
	}
	if kind == "" || kind == analyticsKindActivity {
		rows = append(rows, analyticsActivityRows(data.Snapshot)...)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].At.After(rows[j].At)
	})
	view := analyticsView{
		Kind:     kind,
		Rows:     rows,
		Filtered: kind != "",
		Summary:  formatCount(len(rows)) + " events in the current snapshot.",
	}
	return view
}

func analyticsKindFilter(value string) string {
	switch strings.TrimSpace(value) {
	case analyticsKindAttempts:
		return analyticsKindAttempts
	case analyticsKindActivity:
		return analyticsKindActivity
	}
	return ""
}

func analyticsAttemptRows(snapshot telemetry.Snapshot) []analyticsRow {
	rows := make([]analyticsRow, 0, len(snapshot.WorkAttempts))
	for _, attempt := range snapshot.WorkAttempts {
		at := attempt.StartedAt
		if attempt.HeartbeatAt != nil && attempt.HeartbeatAt.After(at) {
			at = *attempt.HeartbeatAt
		}
		if attempt.CompletedAt != nil && attempt.CompletedAt.After(at) {
			at = *attempt.CompletedAt
		}
		kind, decision := analyticsAttemptDecision(attempt)
		rows = append(rows, analyticsRow{
			ID:       "event-attempt-" + strconv.FormatInt(attempt.AttemptID, 10),
			At:       at,
			Time:     analyticsClock(at),
			Event:    "Work attempt",
			Project:  strings.TrimSpace(attempt.ProjectID),
			Ref:      analyticsRef(attempt.Identifier),
			Kind:     kind,
			Decision: decision,
			Detail:   analyticsAttemptDetail(attempt),
		})
	}
	return rows
}

func analyticsAttemptDecision(attempt telemetry.WorkAttempt) (primitives.Kind, string) {
	if attempt.Stale {
		return primitives.KindWarn, "stale"
	}
	if attempt.ErrorClass != "" || attempt.ErrorMessage != "" {
		return primitives.KindErr, "failed"
	}
	switch strings.ToLower(strings.TrimSpace(attempt.TerminalState)) {
	case "done", "completed", "merged", "success":
		return primitives.KindOK, "completed"
	case "cancelled", "canceled":
		return primitives.KindNeutral, "cancelled"
	}
	switch strings.ToLower(strings.TrimSpace(attempt.Status)) {
	case "running", "active", "dispatched":
		return primitives.KindOK, "dispatched"
	case "skipped":
		return primitives.KindNeutral, "skipped"
	}
	if attempt.Status != "" {
		return primitives.KindNeutral, strings.ToLower(attempt.Status)
	}
	return primitives.KindNeutral, "recorded"
}

func analyticsAttemptDetail(attempt telemetry.WorkAttempt) string {
	parts := make([]string, 0, 3)
	if phase := strings.TrimSpace(attempt.Phase); phase != "" {
		parts = append(parts, "phase "+phase)
	}
	if message := strings.TrimSpace(displayOutputText(attempt.StatusMessage, attempt.StatusMessageTruncation)); message != "" {
		parts = append(parts, message)
	}
	if reason := strings.TrimSpace(attempt.WaitReason); reason != "" {
		parts = append(parts, "waiting on "+reason)
	}
	if errMessage := strings.TrimSpace(attempt.ErrorMessage); errMessage != "" {
		parts = append(parts, errMessage)
	}
	if host := strings.TrimSpace(attempt.WorkerHost); host != "" {
		parts = append(parts, "on "+host)
	}
	if len(parts) == 0 {
		return "attempt " + strconv.Itoa(attempt.AttemptNumber)
	}
	return strings.Join(parts, " · ")
}

func analyticsActivityRows(snapshot telemetry.Snapshot) []analyticsRow {
	rows := make([]analyticsRow, 0, len(snapshot.Events))
	for i, event := range snapshot.Events {
		rows = append(rows, analyticsRow{
			ID:       "event-activity-" + strconv.Itoa(i),
			At:       event.At,
			Time:     analyticsClock(event.At),
			Event:    analyticsEventLabel(event.Event),
			Kind:     primitives.KindNeutral,
			Decision: "recorded",
			Detail:   strings.TrimSpace(event.Message),
		})
	}
	return rows
}

func analyticsEventLabel(event string) string {
	event = strings.TrimSpace(event)
	if event == "" {
		return "Activity"
	}
	return strings.ReplaceAll(event, "_", " ")
}

func analyticsClock(at time.Time) string {
	if at.IsZero() {
		return "—"
	}
	return at.UTC().Format("15:04:05")
}

func analyticsRef(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "—"
	}
	if index := strings.LastIndex(identifier, "#"); index >= 0 && index < len(identifier)-1 {
		return identifier[index:]
	}
	return identifier
}

func analyticsRowClass(last bool) string {
	base := "grid grid-cols-[70px_140px_120px_70px_130px_minmax(0,1fr)] items-center gap-3.5 px-4 py-2"
	if !last {
		return base + " border-b border-line"
	}
	return base
}

func analyticsKindLabel(kind string) string {
	switch kind {
	case analyticsKindAttempts:
		return "Work attempts"
	case analyticsKindActivity:
		return "Activity"
	}
	return "All events"
}

func AnalyticsShellDataFromDashboardV2(data DashboardData) DashboardShellData {
	shell := DashboardShellDataFromDashboard(data)
	shell.ActiveNav = "analytics"
	shell.IncludeDashboardCharts = false
	return shell
}
