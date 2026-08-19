package templates

import (
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	kanbanstate "github.com/digitaldrywood/detent/internal/kanban"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

// boardView is the redesigned home page: exception strip, figure row, and
// fixed-height lanes that scroll independently. Everything is pre-formatted
// so the template stays declarative.
type boardView struct {
	Key         string
	Exceptions  []primitives.Exception
	Figures     []primitives.Figure
	TPS         string
	Spend       string
	Lanes       []boardLaneView
	Visible     int
	Total       int
	HiddenCards int
}

type boardAlertKind string

const (
	boardAlertKindLastKnown              boardAlertKind = "board-last-known"
	boardAlertKindFailureBreaker         boardAlertKind = "project-failure-breaker"
	boardAlertKindDispatchStall          boardAlertKind = "dispatch-stall"
	boardAlertKindTrackerUnavailable     boardAlertKind = "tracker-unavailable"
	boardAlertKindForgeUnavailable       boardAlertKind = "forge-unavailable"
	boardAlertKindCIUnavailable          boardAlertKind = "ci-unavailable"
	boardAlertKindStaleness              boardAlertKind = "staleness-warning"
	boardAlertKindTrackerStale           boardAlertKind = "board-stale-data"
	boardAlertKindAdmissionProposal      boardAlertKind = "admission-proposal"
	boardAlertKindBackendCapacity        boardAlertKind = "backend-capacity-outage"
	boardAlertKindDispatchRecovery       boardAlertKind = "dispatch-recovery-status"
	boardAlertKindUpdatePending          boardAlertKind = "update-pending"
	boardAlertDetailLimit                               = 5
	boardAlertSeverityUpdatePending                     = 100
	boardAlertSeverityDispatchRecovery                  = 200
	boardAlertSeverityBackendCapacity                   = 300
	boardAlertSeverityAdmissionProposal                 = 425
	boardAlertSeverityTrackerStale                      = 400
	boardAlertSeverityStaleness                         = 450
	boardAlertSeverityFailureBreaker                    = 500
	boardAlertSeverityCIUnavailable                     = 550
	boardAlertSeverityTrackerUnavailable                = 560
	boardAlertSeverityForgeUnavailable                  = 565
	boardAlertSeverityDispatchStall                     = 575
	boardAlertSeverityLastKnown                         = 600
)

type boardAlert struct {
	ID            string
	Kind          boardAlertKind
	Severity      int
	Tone          primitives.Kind
	TerseSummary  string
	DetailSummary string
	DetailRows    []boardAlertDetailRow
	Overflow      int
	DeepLink      string
	DeepLinkLabel string
	Action        *boardAlertAction
}

type boardAlertDetailRow struct {
	ID      string
	Label   string
	Link    string
	Summary string
	Detail  string
}

type boardAlertAction struct {
	Label   string
	Path    string
	Target  string
	Confirm string
}

func boardAlerts(snapshot telemetry.Snapshot) []boardAlert {
	alerts := make([]boardAlert, 0, len(snapshot.StalenessWarnings)+8)
	if alert, ok := boardLastKnownAlert(snapshot); ok {
		alerts = append(alerts, alert)
	}
	if alert, ok := boardFailureBreakerAlert(snapshot); ok {
		alerts = append(alerts, alert)
	}
	if alert, ok := boardCIUnavailableAlert(snapshot); ok {
		alerts = append(alerts, alert)
	}
	if alert, ok := boardTrackerUnavailableAlert(snapshot); ok {
		alerts = append(alerts, alert)
	}
	if alert, ok := boardForgeUnavailableAlert(snapshot); ok {
		alerts = append(alerts, alert)
	}
	if alert, ok := boardDispatchStallAlert(snapshot); ok {
		alerts = append(alerts, alert)
	}
	alerts = append(alerts, boardStalenessAlerts(snapshot.StalenessWarnings)...)
	if alert, ok := boardTrackerStaleAlert(snapshot); ok {
		alerts = append(alerts, alert)
	}
	if alert, ok := boardAdmissionProposalAlert(snapshot); ok {
		alerts = append(alerts, alert)
	}
	if alert, ok := boardBackendCapacityAlert(snapshot); ok {
		alerts = append(alerts, alert)
	}
	if alert, ok := boardDispatchRecoveryAlert(snapshot); ok {
		alerts = append(alerts, alert)
	}
	if alert, ok := boardUpdatePendingAlert(snapshot); ok {
		alerts = append(alerts, alert)
	}
	sort.SliceStable(alerts, func(i, j int) bool {
		return alerts[i].Severity > alerts[j].Severity
	})
	return alerts
}

func boardDispatchStallAlert(snapshot telemetry.Snapshot) (boardAlert, bool) {
	if len(snapshot.DispatchStalls) == 0 {
		return boardAlert{}, false
	}
	rows := make([]boardAlertDetailRow, 0, len(snapshot.DispatchStalls))
	for index, stall := range snapshot.DispatchStalls {
		projectID := strings.TrimSpace(stall.ProjectID)
		if projectID == "" {
			projectID = "Fleet"
		}
		detail := boardCountLabel(stall.CandidateCount, "candidate", "candidates") + " skipped for " + formatDuration(float64(stall.StallDurationSeconds))
		if stall.LastSelectedAt != nil {
			detail += " · last dispatch selected " + formatDuration(float64(valueOrZero(stall.SecondsSinceLastSelected))) + " ago"
		}
		rows = append(rows, boardAlertDetailRow{
			ID:      "board-alert-dispatch-stall-" + boardAlertRowSlug(projectID, index),
			Label:   projectID,
			Summary: "Needs human attention",
			Detail:  detail + " · wait reason: " + strings.TrimSpace(stall.WaitReason),
		})
	}
	rows, overflow := capBoardAlertRows(rows)
	return boardAlert{
		ID:            "board-alert-dispatch-stall",
		Kind:          boardAlertKindDispatchStall,
		Severity:      boardAlertSeverityDispatchStall,
		Tone:          primitives.KindErr,
		TerseSummary:  "Dispatch stalled (" + boardCountLabel(len(snapshot.DispatchStalls), "project", "projects") + ")",
		DetailSummary: "Eligible work is not moving and requires an operator decision.",
		DetailRows:    rows,
		Overflow:      overflow,
		DeepLink:      "/health/ui",
	}, true
}

func valueOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func boardCIUnavailableAlert(snapshot telemetry.Snapshot) (boardAlert, bool) {
	if len(snapshot.CIUnavailable) == 0 {
		return boardAlert{}, false
	}
	rows := make([]boardAlertDetailRow, 0, len(snapshot.CIUnavailable))
	for index, condition := range snapshot.CIUnavailable {
		label := strings.TrimSpace(condition.ProjectID)
		if label == "" {
			label = "CI"
		}
		rows = append(rows, boardAlertDetailRow{
			ID:      "board-alert-ci-unavailable-" + boardAlertRowSlug(label, index),
			Label:   label,
			Summary: boardCountLabel(condition.UnstartedCheckCount, "queued check", "queued checks") + " never started",
			Detail:  ciUnavailableConditionDetail(condition),
		})
	}
	rows, overflow := capBoardAlertRows(rows)
	return boardAlert{
		ID:            "board-alert-ci-unavailable",
		Kind:          boardAlertKindCIUnavailable,
		Severity:      boardAlertSeverityCIUnavailable,
		Tone:          primitives.KindErr,
		TerseSummary:  "CI unavailable (" + boardCountLabel(len(snapshot.CIUnavailable), "project", "projects") + ")",
		DetailSummary: "Runner attention required; CI-gated dispatch is paused.",
		DetailRows:    rows,
		Overflow:      overflow,
		DeepLink:      "/health/ui",
	}, true
}

func boardTrackerUnavailableAlert(snapshot telemetry.Snapshot) (boardAlert, bool) {
	if len(snapshot.TrackerUnavailable) == 0 {
		return boardAlert{}, false
	}
	rows := make([]boardAlertDetailRow, 0, len(snapshot.TrackerUnavailable))
	for index, condition := range snapshot.TrackerUnavailable {
		projectLabel := strings.TrimSpace(condition.ProjectID)
		if projectLabel == "" {
			projectLabel = strings.TrimSpace(condition.ConnectorInstance)
		}
		if projectLabel == "" {
			projectLabel = "Tracker"
		}
		summary := strings.TrimSpace(condition.Connector) + " tracker · tracker_unavailable"
		if strings.TrimSpace(condition.Connector) == "" {
			summary = "Tracker · tracker_unavailable"
		}
		detail := strings.TrimSpace(condition.Operation) + " · " + strings.TrimSpace(condition.ErrorClass)
		if !condition.NextProbeAt.IsZero() {
			delay := condition.NextProbeAt.Sub(snapshot.GeneratedAt)
			if delay < 0 {
				delay = 0
			}
			detail += " · next canary in " + formatDuration(delay.Seconds())
		}
		label := projectLabel
		link := ""
		if providerSummary, providerLink := trackerProviderStatusSummary(condition); providerSummary != "" {
			summary = providerSummary
			if providerLink != "" {
				label = providerSummary
				link = providerLink
				summary = projectLabel
			}
			if condition.ProviderStatus != nil && condition.ProviderStatus.Incident != nil {
				detail = condition.ProviderStatus.Incident.Name + " · " + detail
			}
		}
		rows = append(rows, boardAlertDetailRow{
			ID:      "board-alert-tracker-unavailable-" + boardAlertRowSlug(projectLabel, index),
			Label:   label,
			Link:    link,
			Summary: summary,
			Detail:  strings.Trim(detail, " ·"),
		})
	}
	rows, overflow := capBoardAlertRows(rows)
	alert := boardAlert{
		ID:            "board-alert-tracker-unavailable",
		Kind:          boardAlertKindTrackerUnavailable,
		Severity:      boardAlertSeverityTrackerUnavailable,
		Tone:          primitives.KindErr,
		TerseSummary:  "Tracker unavailable (" + boardCountLabel(len(snapshot.TrackerUnavailable), "connector", "connectors") + ")",
		DetailSummary: "Tracker-dependent dispatch is paused until a canary read succeeds.",
		DetailRows:    rows,
		Overflow:      overflow,
		DeepLink:      "/health/ui",
	}
	if len(snapshot.TrackerUnavailable) == 1 {
		projectID := strings.TrimSpace(snapshot.TrackerUnavailable[0].ProjectID)
		path := "/api/v1/tracker/availability/clear"
		if projectID != "" {
			path += "?project_id=" + url.QueryEscape(projectID)
		}
		alert.Action = &boardAlertAction{
			Label:   "Clear condition",
			Path:    path,
			Target:  "#board-alert-tracker-unavailable",
			Confirm: "Clear the tracker availability condition and resume dispatch?",
		}
	}
	return alert, true
}

func boardForgeUnavailableAlert(snapshot telemetry.Snapshot) (boardAlert, bool) {
	if len(snapshot.ForgeUnavailable) == 0 {
		return boardAlert{}, false
	}
	rows := make([]boardAlertDetailRow, 0, len(snapshot.ForgeUnavailable))
	for index, condition := range snapshot.ForgeUnavailable {
		label := strings.TrimSpace(condition.ProjectID)
		if label == "" {
			label = strings.TrimSpace(condition.Host)
		}
		if label == "" {
			label = "Forge"
		}
		host := strings.TrimSpace(condition.Host)
		if host == "" {
			host = "Configured"
		}
		detail := strings.Trim(strings.TrimSpace(condition.Operation)+" · "+strings.TrimSpace(condition.ErrorClass), " ·")
		if !condition.NextProbeAt.IsZero() {
			delay := condition.NextProbeAt.Sub(snapshot.GeneratedAt)
			if delay < 0 {
				delay = 0
			}
			detail += " · next write canary in " + formatDuration(delay.Seconds())
		}
		rows = append(rows, boardAlertDetailRow{
			ID:      "board-alert-forge-unavailable-" + boardAlertRowSlug(label, index),
			Label:   label,
			Summary: host + " forge · forge_unavailable",
			Detail:  strings.Trim(detail, " ·"),
		})
	}
	rows, overflow := capBoardAlertRows(rows)
	alert := boardAlert{
		ID:            "board-alert-forge-unavailable",
		Kind:          boardAlertKindForgeUnavailable,
		Severity:      boardAlertSeverityForgeUnavailable,
		Tone:          primitives.KindErr,
		TerseSummary:  "Forge writes unavailable (" + boardCountLabel(len(snapshot.ForgeUnavailable), "host", "hosts") + ")",
		DetailSummary: "Push and pull-request delivery are paused until a write canary succeeds; other work can proceed.",
		DetailRows:    rows,
		Overflow:      overflow,
		DeepLink:      "/health/ui",
	}
	if len(snapshot.ForgeUnavailable) == 1 {
		condition := snapshot.ForgeUnavailable[0]
		query := url.Values{}
		if projectID := strings.TrimSpace(condition.ProjectID); projectID != "" {
			query.Set("project_id", projectID)
		}
		if host := strings.TrimSpace(condition.Host); host != "" {
			query.Set("host", host)
		}
		path := "/api/v1/forge/availability/clear"
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
		}
		alert.Action = &boardAlertAction{
			Label:   "Clear condition",
			Path:    path,
			Target:  "#board-alert-forge-unavailable",
			Confirm: "Clear the forge availability condition and allow write delivery to resume?",
		}
	}
	return alert, true
}

func ciUnavailableConditionDetail(condition telemetry.CICondition) string {
	detail := "across " + boardCountLabel(condition.PullRequestCount, "PR", "PRs")
	if condition.OldestQueueSeconds > 0 {
		detail += " · oldest queued " + formatDuration(float64(condition.OldestQueueSeconds))
	}
	if condition.ParkedAttemptCount > 0 {
		detail += " · " + boardCountLabel(condition.ParkedAttemptCount, "attempt parked", "attempts parked")
	}
	return detail
}

func boardAdmissionProposalAlert(snapshot telemetry.Snapshot) (boardAlert, bool) {
	if len(snapshot.AdmissionProposals) == 0 {
		return boardAlert{}, false
	}
	rows := make([]boardAlertDetailRow, 0, len(snapshot.AdmissionProposals))
	for index, proposal := range snapshot.AdmissionProposals {
		rows = append(rows, boardAlertDetailRow{
			ID:      "board-alert-admission-" + boardAlertRowSlug(proposal.ID, index),
			Label:   admissionProposalTarget(proposal),
			Link:    strings.TrimSpace(proposal.IssueURL),
			Summary: strings.TrimSpace(proposal.ProjectID),
			Detail:  admissionProposalTiming(proposal, snapshot.GeneratedAt),
		})
	}
	total := len(rows)
	rows, overflow := capBoardAlertRows(rows)
	return boardAlert{
		ID:            "board-alert-admission-proposals",
		Kind:          boardAlertKindAdmissionProposal,
		Severity:      boardAlertSeverityAdmissionProposal,
		Tone:          primitives.KindWarn,
		TerseSummary:  boardCountLabel(total, "admission proposal awaiting decision", "admission proposals awaiting decision"),
		DetailSummary: "Human admission decisions are required.",
		DetailRows:    rows,
		Overflow:      overflow,
		DeepLink:      "/health/ui",
	}, true
}

func admissionProposalTarget(proposal telemetry.AdmissionProposal) string {
	for _, value := range []string{proposal.IssueIdentifier, proposal.IssueID} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "issue"
}

func admissionProposalTiming(proposal telemetry.AdmissionProposal, observedAt time.Time) string {
	age := max(observedAt.Sub(proposal.CreatedAt), 0)
	timeToExpiry := max(proposal.ExpiresAt.Sub(observedAt), 0)
	return formatContextPercent(proposal.Confidence*100) + " confidence · age " +
		formatDuration(age.Seconds()) + " · expires in " + formatDuration(timeToExpiry.Seconds())
}

func boardStalenessAlerts(warnings []telemetry.StalenessWarning) []boardAlert {
	alerts := make([]boardAlert, 0, len(warnings))
	for index, warning := range warnings {
		target := strings.TrimSpace(warning.Identifier)
		if target == "" {
			target = strings.TrimSpace(warning.ProjectID)
		}
		summary := stalenessExceptionTitle(warning)
		if target != "" {
			summary += " · " + target
		}
		detail := strings.TrimSpace(warning.Detail)
		if warning.AgeSeconds > 0 {
			detail += " · " + formatDuration(float64(warning.AgeSeconds))
		}
		alerts = append(alerts, boardAlert{
			ID:            "board-alert-staleness-" + boardAlertRowSlug(warning.ID, index),
			Kind:          boardAlertKindStaleness,
			Severity:      boardAlertSeverityStaleness,
			Tone:          primitives.KindWarn,
			TerseSummary:  summary,
			DetailSummary: detail,
			DeepLink:      strings.TrimSpace(warning.IssueURL),
			DeepLinkLabel: "Open",
		})
	}
	return alerts
}

func boardLastKnownAlert(snapshot telemetry.Snapshot) (boardAlert, bool) {
	if !snapshot.LastKnown || snapshotUsesStartupCache(snapshot) {
		return boardAlert{}, false
	}
	tone := primitives.KindWarn
	detailSummary := "Tracker refresh is behind; no refresh failure is reported."
	detail := "The board is showing cached state while tracker refresh continues."
	if refreshSnapshotFailed(snapshot) {
		tone = primitives.KindErr
		detailSummary = "The live board snapshot is unavailable because tracker refresh failed."
		detail = "The board is showing cached state until tracker refresh recovers."
	}
	return boardAlert{
		ID:            "board-alert-last-known",
		Kind:          boardAlertKindLastKnown,
		Severity:      boardAlertSeverityLastKnown,
		Tone:          tone,
		TerseSummary:  "Board showing last-known state",
		DetailSummary: detailSummary,
		DetailRows: []boardAlertDetailRow{{
			ID:      "board-alert-last-known-snapshot",
			Label:   "Snapshot",
			Summary: "Cached board state",
			Detail:  detail,
		}},
		DeepLink: "/health/ui",
	}, true
}

func refreshSnapshotFailed(snapshot telemetry.Snapshot) bool {
	if refreshFailed(snapshot.Refresh) {
		return true
	}
	for _, project := range snapshot.Projects {
		if refreshFailed(project.Refresh) {
			return true
		}
	}
	return false
}

func refreshFailed(refresh telemetry.Refresh) bool {
	if !refresh.Degraded() || refresh.StalenessWindowExceeded {
		return false
	}
	if strings.TrimSpace(refresh.LastError) != "" {
		return true
	}
	threshold := refresh.FailureThreshold
	if threshold <= 0 {
		threshold = 3
	}
	for _, source := range refresh.Sources {
		if source.Degraded || source.FailureStreak >= threshold {
			return true
		}
	}
	return false
}

func boardFailureBreakerAlert(snapshot telemetry.Snapshot) (boardAlert, bool) {
	summary, ok := boardFailureBreakerSummary(snapshot.FailureBreakers)
	if !ok {
		return boardAlert{}, false
	}
	rows := make([]boardAlertDetailRow, 0, len(snapshot.FailureBreakers))
	for index, breaker := range snapshot.FailureBreakers {
		projectID := strings.TrimSpace(breaker.ProjectID)
		label := projectID
		if label == "" {
			label = "Project"
		}
		link := ""
		if len(breaker.Items) == 1 {
			if itemLabel := failureBreakerItemLabel(breaker.Items[0]); itemLabel != "" {
				label = itemLabel
			}
			link = strings.TrimSpace(breaker.Items[0].IssueURL)
		}
		detail, detailAt, showDetailAt := failureBreakerDetailParts(breaker, snapshot.GeneratedAt)
		if showDetailAt && !detailAt.IsZero() {
			detail += " " + localTimeToken(detailAt, LocalDateTimeZone)
			if !snapshot.GeneratedAt.IsZero() && detailAt.After(snapshot.GeneratedAt) {
				detail += " (in " + formatDuration(detailAt.Sub(snapshot.GeneratedAt).Seconds()) + ")"
			}
			detail += "."
		}
		rows = append(rows, boardAlertDetailRow{
			ID:      "board-alert-failure-breaker-" + boardAlertRowSlug(projectID+"-"+breaker.Class, index),
			Label:   label,
			Link:    link,
			Summary: failureBreakerCauseLabel(breaker),
			Detail:  detail,
		})
	}
	rows, overflow := capBoardAlertRows(rows)
	count := boardAffectedProjectCount(len(snapshot.FailureBreakers), func(yield func(string)) {
		for _, breaker := range snapshot.FailureBreakers {
			yield(breaker.ProjectID)
		}
	})
	return boardAlert{
		ID:            "board-alert-failure-breaker",
		Kind:          boardAlertKindFailureBreaker,
		Severity:      boardAlertSeverityFailureBreaker,
		Tone:          primitives.KindErr,
		TerseSummary:  "Project failure breaker (" + boardCountLabel(count, "project", "projects") + ")",
		DetailSummary: summary.Title,
		DetailRows:    rows,
		Overflow:      overflow,
		DeepLink:      "/health/ui",
	}, true
}

func boardTrackerStaleAlert(snapshot telemetry.Snapshot) (boardAlert, bool) {
	if snapshot.LastKnown {
		return boardAlert{}, false
	}
	if refreshFreshnessKind(snapshot) != primitives.KindWarn {
		return boardAlert{}, false
	}
	details := refreshStaleDetailRows(snapshot)
	rows := make([]boardAlertDetailRow, 0, len(details))
	for index, detail := range details {
		rows = append(rows, boardAlertDetailRow{
			ID:      "board-alert-tracker-" + boardAlertRowSlug(detail.ProjectID, index),
			Label:   detail.ProjectID,
			Summary: detail.Sources,
			Detail:  detail.Detail,
		})
	}
	rows, overflow := capBoardAlertRows(rows)
	summary := refreshStaleSummary(snapshot)
	terse := "Tracker stale"
	if projectCount := refreshStaleProjectCount(snapshot); projectCount > 0 {
		terse += " (" + boardCountLabel(projectCount, "project", "projects") + ")"
	}
	return boardAlert{
		ID:            "board-alert-tracker-stale",
		Kind:          boardAlertKindTrackerStale,
		Severity:      boardAlertSeverityTrackerStale,
		Tone:          primitives.KindWarn,
		TerseSummary:  terse,
		DetailSummary: summary,
		DetailRows:    rows,
		Overflow:      overflow,
		DeepLink:      "/health/ui",
	}, true
}

func boardBackendCapacityAlert(snapshot telemetry.Snapshot) (boardAlert, bool) {
	summaries := boardBackendCapacitySummaries(snapshot.BackendOutages, snapshot.GeneratedAt)
	if len(summaries) == 0 {
		return boardAlert{}, false
	}
	rows := make([]boardAlertDetailRow, 0, len(snapshot.BackendOutages))
	for index, outage := range backendCapacityOutageDetails(snapshot.BackendOutages) {
		title, selected := boardBackendCapacityTitle(outage, snapshot.GeneratedAt)
		if !selected {
			continue
		}
		label := strings.TrimSpace(outage.ProjectID)
		if label == "" {
			label = backendCapacityBackendID(outage)
		}
		detail, detailAt, showDetailAt := backendCapacityOutageDetailParts(outage, snapshot.GeneratedAt)
		rows = append(rows, boardAlertDetailRow{
			ID:      "board-alert-backend-capacity-" + boardAlertRowSlug(boardAlertBackendCapacityRowKey(outage), index),
			Label:   label,
			Summary: title,
			Detail:  boardAlertDetailWithTime(detail, detailAt, snapshot.GeneratedAt, showDetailAt),
		})
	}
	rows, overflow := capBoardAlertRows(rows)
	return boardAlert{
		ID:            "board-alert-backend-capacity",
		Kind:          boardAlertKindBackendCapacity,
		Severity:      boardAlertSeverityBackendCapacity,
		Tone:          primitives.KindWarn,
		TerseSummary:  summaries[0].Title,
		DetailSummary: boardCountLabel(len(summaries), "capacity issue", "capacity issues"),
		DetailRows:    rows,
		Overflow:      overflow,
		DeepLink:      "/health/ui",
	}, true
}

func boardDispatchRecoveryAlert(snapshot telemetry.Snapshot) (boardAlert, bool) {
	summaries := boardDispatchRecoverySummaries(snapshot.DispatchRecoveries, snapshot.GeneratedAt)
	if len(summaries) == 0 {
		return boardAlert{}, false
	}
	rows := make([]boardAlertDetailRow, 0, len(snapshot.DispatchRecoveries))
	for index, recovery := range snapshot.DispatchRecoveries {
		title, selected := boardDispatchRecoveryAlertTitle(recovery, snapshot.GeneratedAt)
		if !selected {
			continue
		}
		label := strings.TrimSpace(recovery.ProjectID)
		if label == "" {
			label = "Dispatch"
		}
		detail, detailAt, showDetailAt := dispatchRecoveryDetailParts(recovery, snapshot.GeneratedAt)
		rows = append(rows, boardAlertDetailRow{
			ID:      "board-alert-dispatch-recovery-" + boardAlertRowSlug(label+"-"+recovery.Kind+"-"+recovery.Status, index),
			Label:   label,
			Summary: title,
			Detail:  boardAlertDetailWithTime(detail, detailAt, snapshot.GeneratedAt, showDetailAt),
		})
	}
	rows, overflow := capBoardAlertRows(rows)
	return boardAlert{
		ID:            "board-alert-dispatch-recovery",
		Kind:          boardAlertKindDispatchRecovery,
		Severity:      boardAlertSeverityDispatchRecovery,
		Tone:          primitives.KindWarn,
		TerseSummary:  summaries[0].Title,
		DetailSummary: boardCountLabel(len(summaries), "recovery issue", "recovery issues"),
		DetailRows:    rows,
		Overflow:      overflow,
		DeepLink:      "/health/ui",
	}, true
}

func boardUpdatePendingAlert(snapshot telemetry.Snapshot) (boardAlert, bool) {
	if !detentUpdatePending(snapshot.Update) {
		return boardAlert{}, false
	}
	version := detentPendingUpdateVersion(snapshot.Update)
	target := "board-alert-update-pending-status"
	return boardAlert{
		ID:            "board-alert-update-pending",
		Kind:          boardAlertKindUpdatePending,
		Severity:      boardAlertSeverityUpdatePending,
		Tone:          primitives.KindInfo,
		TerseSummary:  "Detent " + version + " pending",
		DetailSummary: "A Detent update is ready to apply.",
		DetailRows: []boardAlertDetailRow{{
			ID:      target,
			Label:   "Update",
			Summary: version,
			Detail:  "Automatic apply is waiting for all active work attempts across every project to finish.",
		}},
		Action: &boardAlertAction{
			Label:   "Apply now",
			Path:    "/api/v1/update/apply",
			Target:  "#" + target,
			Confirm: "Apply the update now? Detent will drain active attempts and restart.",
		},
	}, true
}

func boardDispatchRecoveryAlertTitle(recovery telemetry.DispatchRecovery, now time.Time) (string, bool) {
	status := strings.TrimSpace(recovery.Status)
	switch status {
	case "ramping":
		return "", false
	case "waiting":
		if automaticRecoveryPending(recovery.ResumeAt, now) {
			return "", false
		}
		kind := dispatchRecoveryKindLabel(recovery.Kind)
		if automaticRecoveryOverdue(recovery.ResumeAt, now) {
			return "Dispatch retry overdue for " + kind, true
		}
		return "Dispatch waiting on " + kind, true
	default:
		return "Dispatch recovery requires attention for " + dispatchRecoveryKindLabel(recovery.Kind), true
	}
}

func boardAlertDetailWithTime(detail string, detailAt time.Time, now time.Time, include bool) string {
	if !include || detailAt.IsZero() || now.IsZero() {
		return detail
	}
	if detailAt.After(now) {
		return detail + " in " + formatDuration(detailAt.Sub(now).Seconds()) + "."
	}
	return detail + " now."
}

func capBoardAlertRows(rows []boardAlertDetailRow) ([]boardAlertDetailRow, int) {
	if len(rows) <= boardAlertDetailLimit {
		return rows, 0
	}
	return rows[:boardAlertDetailLimit], len(rows) - boardAlertDetailLimit
}

func boardAlertRowSlug(value string, index int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "row-" + strconv.Itoa(index+1)
	}
	return boardCardSlug(value)
}

func boardAlertBackendCapacityRowKey(outage telemetry.BackendOutage) string {
	resetAt := outage.ResumeAt
	if outage.ResetAt != nil && !outage.ResetAt.IsZero() {
		resetAt = *outage.ResetAt
	}
	return strings.TrimSpace(outage.ProjectID) + "-" + backendCapacityBackendID(outage) + "-" + strings.TrimSpace(outage.Kind) + "-" + localTimeISOString(resetAt)
}

func boardAlertKindActive(alerts []boardAlert, kind boardAlertKind) bool {
	for _, alert := range alerts {
		if alert.Kind == kind {
			return true
		}
	}
	return false
}

func boardAlertButtonLabel(alerts []boardAlert) string {
	if len(alerts) == 0 {
		return "No board warnings"
	}
	return boardCountLabel(len(alerts), "board warning", "board warnings") + ". Highest severity: " + alerts[0].TerseSummary + ". Expand details."
}

func boardAlertsClass(alerts []boardAlert) string {
	class := "min-w-0 max-w-full self-center overflow-hidden rounded-chip border"
	if len(alerts) == 0 {
		return class
	}
	switch alerts[0].Tone {
	case primitives.KindErr:
		return class + " border-err/40 bg-err/10 text-err"
	case primitives.KindInfo:
		return class + " border-info/40 bg-info/10 text-info"
	default:
		return class + " border-warn/40 bg-warn/10 text-warn"
	}
}

type boardLaneView struct {
	DomID          string
	LaneID         string
	Title          string
	DropState      string
	DropKey        string
	Count          string
	CardCount      int
	Live           bool
	DefaultVisible bool
	EmptyMessage   string
	Cards          []boardCardView
}

const (
	boardLaneVisibilityStoragePrefix       = "detent.ui.board.lanes.v2."
	boardLaneVisibilityLegacyStoragePrefix = "detent.ui.board.lanes."
	boardLaneVisibilityStorageVersion      = 1
)

type boardLaneVisibilityState string

const (
	boardLaneVisibilityAuto boardLaneVisibilityState = "auto"
	boardLaneVisibilityShow boardLaneVisibilityState = "show"
	boardLaneVisibilityHide boardLaneVisibilityState = "hide"
)

type boardLaneVisibilityPrefs struct {
	Show map[string]struct{}
	Hide map[string]struct{}
}

type boardLaneVisibilityPayload struct {
	Version int      `json:"v"`
	Show    []string `json:"show,omitempty"`
	Hide    []string `json:"hide,omitempty"`
}

// boardCardView preformats the shared and density-specific card fields.
type boardCardView struct {
	DomID             string
	Identity          string
	IssueID           string
	Number            string
	URL               string
	Project           string
	MoveProject       string
	Scope             string
	CurrentState      string
	DataSeq           uint64
	PRNumber          string
	PRURL             string
	DragDrop          bool
	CanDrag           bool
	AllowedTargets    string
	MoveDisabledText  string
	MoveDisabledLabel string
	Running           bool
	Retrying          bool
	Waiting           bool
	Done              bool
	Terminal          bool
	MetaRight         string
	AgeFooter         string
	AgeFooterTitle    string
	Title             string
	State             string
	Origin            string
	OriginDetail      string
	AuthorDetail      string
	CompactSignal     string
	ExtraKind         primitives.Kind
	ExtraText         string
	ExtraChip         bool
	RuntimeSummary    string
	RuntimeCozyText   string
	RuntimeComfyText  string
	RuntimeDetail     string
	RuntimeBadge      bool
	ParkSummary       string
	ParkDetail        string
	ProgressSummary   string
	ProgressDetail    string
	ProgressKind      primitives.Kind
	BlockerSummary    string
	Labels            []string
	Effort            string
	Activity          string
	PRStatus          string
	PRStatusClass     string
	PriorityBadge     string
	PriorityTitle     string
	PriorityDetail    string
	PriorityTop       bool
	MergeLaneStatus   string
	MergeLaneDetail   string
	MergeLaneKind     primitives.Kind
}

func boardViewFromDashboard(data DashboardData) boardView {
	board := projectKanbanBoardView(data)
	spend := "-- today"
	if !data.PendingEnrichment {
		spend = formatUSD(data.Snapshot.Budget.CurrentSpendUSD) + " notional USD today"
	}
	view := boardView{
		Key:        boardVisibilityKey(data),
		Exceptions: boardExceptions(data, true),
		Figures:    boardFiguresFromDashboard(data),
		TPS:        throughputRate(data.Snapshot),
		Spend:      spend,
	}
	fallbackProjectID := boardFallbackProjectID(data)
	globalTerminalStates := projectKanbanTerminalStateSet(data.Kanban.TerminalStates)
	// An entirely empty board shows its non-terminal lanes so the operator
	// sees the empty states rather than a blank strip; once any lane has
	// cards, empty lanes collapse to reduce clutter.
	boardHasCards := false
	for _, lane := range board.AllLanes {
		if len(lane.Cards) > 0 {
			boardHasCards = true
			break
		}
	}
	dragDrop := projectKanbanDragDropEnabled(data)
	for _, lane := range board.AllLanes {
		// The fleet board mixes projects, so a lane's terminal-ness is
		// resolved per card. A populated lane counts as terminal only when it
		// is terminal for every card's own project; an empty lane falls back
		// to the global set.
		laneTerminal := boardLaneTerminal(data, lane, globalTerminalStates)
		liveCount := 0
		for _, card := range lane.Cards {
			if boardCardIsRunning(data.Snapshot, card) {
				liveCount++
			}
		}
		inProgress := strings.EqualFold(lane.Title, "In Progress")
		count := formatCount(len(lane.Cards))
		if inProgress {
			switch {
			case runtimeCountComplete(data.Snapshot):
				count += " (" + formatCount(liveCount) + " live)"
			case liveCount > 0:
				count += " (" + formatCount(liveCount) + "+ live)"
			default:
				count += " (live unknown)"
			}
		}
		laneView := boardLaneView{
			DomID:     "lane-" + lane.ID,
			LaneID:    lane.ID,
			Title:     lane.Title,
			Count:     count,
			CardCount: len(lane.Cards),
			Live:      inProgress && liveCount > 0,
			// Populated lanes show by default, except terminal graveyards
			// (Done, Cancelled, Closed, …), which remain reachable via the picker.
			DefaultVisible: boardLaneDefaultVisible(lane, laneTerminal, boardHasCards),
			EmptyMessage:   "No issues in " + lane.Title,
		}
		if dragDrop {
			laneView.DropState = lane.Title
			laneView.DropKey = projectKanbanStateKey(lane.Title)
		}
		for _, card := range lane.Cards {
			cardTerminal := projectKanbanTerminalState(lane.Title, projectKanbanTerminalStateSetForProject(data, card.ProjectID))
			laneView.Cards = append(laneView.Cards, boardCardViewFromCard(data, lane, card, cardTerminal, projectKanbanBoardScope(data), fallbackProjectID))
		}
		view.Lanes = append(view.Lanes, laneView)
		view.Total++
		if laneView.DefaultVisible {
			view.Visible++
		} else if laneView.CardCount > 0 {
			view.HiddenCards += laneView.CardCount
		}
	}
	return view
}

func boardLaneVisibilityPrefsFromStorage(raw string) (boardLaneVisibilityPrefs, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return boardLaneVisibilityPrefs{}, false
	}
	var payload boardLaneVisibilityPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return boardLaneVisibilityPrefs{}, true
	}
	if payload.Version != boardLaneVisibilityStorageVersion {
		return boardLaneVisibilityPrefs{}, true
	}
	return boardLaneVisibilityPrefsFromLists(payload.Show, payload.Hide), false
}

func boardLaneVisibilityPrefsFromLists(show []string, hide []string) boardLaneVisibilityPrefs {
	prefs := boardLaneVisibilityPrefs{
		Show: map[string]struct{}{},
		Hide: map[string]struct{}{},
	}
	for _, id := range show {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		prefs.Show[id] = struct{}{}
	}
	for _, id := range hide {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := prefs.Show[id]; ok {
			continue
		}
		prefs.Hide[id] = struct{}{}
	}
	return prefs
}

func boardLaneVisibilityStateForLane(prefs boardLaneVisibilityPrefs, laneID string) boardLaneVisibilityState {
	if _, ok := prefs.Show[laneID]; ok {
		return boardLaneVisibilityShow
	}
	if _, ok := prefs.Hide[laneID]; ok {
		return boardLaneVisibilityHide
	}
	return boardLaneVisibilityAuto
}

func boardLaneVisibilityResolve(defaultVisible bool, state boardLaneVisibilityState) bool {
	switch state {
	case boardLaneVisibilityShow:
		return true
	case boardLaneVisibilityHide:
		return false
	default:
		return defaultVisible
	}
}

// boardFallbackProjectID resolves the project a card belongs to when its
// Issue.ProjectID is empty (legacy single-project snapshots): the scoped
// dashboard project, then the snapshot project, then the sole configured
// project. Without it the sheet request omits project scope and eligible
// cards lose their Move action on the home board.
func boardFallbackProjectID(data DashboardData) string {
	if id := strings.TrimSpace(data.ProjectID); id != "" {
		return id
	}
	if id := strings.TrimSpace(data.Snapshot.Project.ID); id != "" {
		return id
	}
	if len(data.Projects) == 1 {
		return strings.TrimSpace(data.Projects[0].ID)
	}
	return ""
}

// boardLaneTerminal reports whether a lane should be treated as a terminal
// graveyard. A populated lane is terminal only when it is terminal for every
// card's own project; an empty lane uses the global terminal set.
func boardLaneTerminal(data DashboardData, lane projectKanbanLane, globalTerminalStates map[string]struct{}) bool {
	if len(lane.Cards) == 0 {
		return projectKanbanTerminalState(lane.Title, globalTerminalStates)
	}
	for _, card := range lane.Cards {
		if !projectKanbanTerminalState(lane.Title, projectKanbanTerminalStateSetForProject(data, card.ProjectID)) {
			return false
		}
	}
	return true
}

func boardLaneDefaultVisible(lane projectKanbanLane, terminal bool, boardHasCards bool) bool {
	if !boardHasCards {
		// Empty board: keep non-terminal lanes visible so their empty
		// states are legible instead of a blank strip.
		return !terminal
	}
	return len(lane.Cards) > 0 && !terminal
}

func boardVisibilityKey(data DashboardData) string {
	if id := strings.TrimSpace(data.ProjectID); id != "" {
		return "project." + id
	}
	return "fleet"
}

func boardFigures(snapshot telemetry.Snapshot) []primitives.Figure {
	workload := telemetry.BoardWorkload(snapshot)
	return []primitives.Figure{
		{ID: "fig-running", Value: runningCountLabel(snapshot), Label: "running"},
		{ID: "fig-ready", Value: formatCount(workload.Todo), Label: "ready"},
		{ID: "fig-waiting", Value: formatCount(workload.Waiting), Label: "waiting"},
		{ID: "fig-blocked", Value: formatCount(workload.Blocked), Label: "blocked", Err: workload.Blocked > 0},
		{ID: "fig-completed", Value: formatCount(completedCount(snapshot)), Label: "completed"},
	}
}

func boardFiguresFromDashboard(data DashboardData) []primitives.Figure {
	figures := boardFigures(data.Snapshot)
	figures[len(figures)-1].Value = formatCount(len(projectKanbanRecentCompletions(data)))
	figures[len(figures)-1].Label = "completed · 48h"
	return figures
}

// boardExceptions builds the exception strip. boardActions is true only
// when the calling page renders a board into #snapshot (Board, project
// Kanban), so the Review sheet may offer inline Move/Remove; the Fleet and
// Overview pages pass false so their Review sheets stay read-only.
func boardExceptions(data DashboardData, boardActions bool) []primitives.Exception {
	var exceptions []primitives.Exception
	if !boardActions {
		exceptions = stalenessExceptions(data.Snapshot)
	}
	if boardActions && !data.Kanban.ShowBlockedAlerts {
		return exceptions
	}

	retryRows := make([]telemetry.Blocked, 0, len(data.Snapshot.Blocked))
	reviewRows := make([]telemetry.Blocked, 0, len(data.Snapshot.Blocked))
	for _, row := range data.Snapshot.Blocked {
		if boardBlockedWaiting(row.Source, row.RecoveryAction, row.RecoveryReason, row.Error) {
			continue
		}
		if StopRunRetryDialogPath(row, data.ProjectID) != "" {
			retryRows = append(retryRows, row)
			continue
		}
		reviewRows = append(reviewRows, row)
	}
	if len(retryRows) == 0 && len(reviewRows) == 0 {
		return exceptions
	}
	for _, row := range retryRows {
		exceptions = append(exceptions, boardOperatorStopException(data, row))
	}
	if len(reviewRows) > 0 {
		exceptions = append(exceptions, boardBlockedExceptionSummary(data, reviewRows, boardActions))
	}
	return exceptions
}

func stalenessExceptions(snapshot telemetry.Snapshot) []primitives.Exception {
	exceptions := make([]primitives.Exception, 0, len(snapshot.StalenessWarnings))
	for _, warning := range snapshot.StalenessWarnings {
		repo, ref := splitIssueIdentifier(strings.TrimSpace(warning.Identifier))
		if repo == "" {
			repo = strings.TrimSpace(warning.ProjectID)
		}
		rest := strings.TrimSpace(warning.Detail)
		if warning.AgeSeconds > 0 {
			rest += " · " + formatDuration(float64(warning.AgeSeconds))
		}
		exception := primitives.Exception{
			ID:     "exception-staleness-" + boardCardSlug(warning.ID),
			Kind:   primitives.KindWarn,
			Title:  stalenessExceptionTitle(warning),
			Repo:   repo,
			Ref:    ref,
			RefURL: strings.TrimSpace(warning.IssueURL),
			Rest:   rest,
		}
		if exception.RefURL != "" {
			exception.ActionLabel = "Open"
			exception.ActionHref = exception.RefURL
		}
		exceptions = append(exceptions, exception)
	}
	return exceptions
}

func stalenessExceptionTitle(warning telemetry.StalenessWarning) string {
	switch warning.Kind {
	case "project_liveness":
		return "Project is not advancing"
	case "merge_liveness":
		return "Merge queue is not advancing"
	case "repeated_decision":
		return "Scheduler decision is repeating"
	default:
		if warning.WaitingOnHuman {
			return "Human gate needs a reminder"
		}
		return "Work item is stale"
	}
}

func boardOperatorStopException(data DashboardData, row telemetry.Blocked) primitives.Exception {
	projectID := strings.TrimSpace(row.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(data.ProjectID)
	}
	identity := boardCardIdentityToken(row.Identifier, row.ID, projectKanbanIssueNumber(row.Issue))
	return primitives.Exception{
		ID:          "exception-" + boardCardScopedSlug(projectID, identity),
		Kind:        primitives.KindErr,
		Title:       "Run stopped; routing failed",
		Repo:        projectID,
		Ref:         projectKanbanIssueNumber(row.Issue),
		RefURL:      strings.TrimSpace(row.URL),
		Rest:        boardExceptionDetail(row, pipelineNow(data.Snapshot)),
		ActionLabel: "Retry routing",
		ActionAttrs: templ.Attributes{
			"hx-get":                       StopRunRetryDialogPath(row, data.ProjectID),
			"hx-target":                    kanbanDialogTargetSelector(),
			"hx-swap":                      "innerHTML",
			"data-tui-dialog-trigger":      true,
			"data-tui-dialog-target":       kanbanActionDialogID,
			"data-tui-dialog-trigger-open": "false",
		},
	}
}

func boardBlockedExceptionSummary(data DashboardData, rows []telemetry.Blocked, boardActions bool) primitives.Exception {
	row := rows[0]
	projectID := strings.TrimSpace(row.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(data.ProjectID)
	}
	identity := boardCardIdentityToken(row.Identifier, row.ID, projectKanbanIssueNumber(row.Issue))
	exception := primitives.Exception{
		ID:     "exception-" + boardCardScopedSlug(projectID, identity),
		Kind:   primitives.KindErr,
		Title:  "Needs review",
		Repo:   projectID,
		Ref:    projectKanbanIssueNumber(row.Issue),
		RefURL: strings.TrimSpace(row.URL),
		Rest:   boardExceptionDetail(row, pipelineNow(data.Snapshot)),
	}
	if len(rows) == 1 {
		exception.ActionLabel = "Review"
		exception.ActionAttrs = sheetOpenAttrs(projectID, identity, projectKanbanBoardScope(data), boardActions)
		return exception
	}
	exception.ID = "exception-blocked-review"
	exception.Title = formatCount(len(rows)) + " blocked items need review"
	exception.Rest = strings.TrimSpace(exception.Rest + " · " + boardMoreBlockedLabel(len(rows)-1))
	return exception
}

func boardMoreBlockedLabel(count int) string {
	if count == 1 {
		return "plus 1 more"
	}
	return "plus " + formatCount(count) + " more"
}

func boardExceptionDetail(row telemetry.Blocked, now time.Time) string {
	detail := boardBlockedDetail(row.Source, row.RecoveryAction, row.RecoveryReason, row.RecoveryRemedy, row.Error)
	if evidence := boardBlockerEvidenceDetail(row, now); evidence != "" {
		detail = strings.TrimSpace(detail + " · " + evidence)
	}
	if detail == "" && boardBlockedWaiting(row.Source, row.RecoveryAction, row.RecoveryReason, row.Error) {
		if boardBlockedDependencyWaiting(row.Source, row.RecoveryReason, row.Error, row.BlockedBy) {
			detail = "dependency not ready"
		} else {
			detail = "paused by project status"
		}
	}
	if detail == "" {
		detail = "needs operator attention"
	}
	if row.BlockedAt != nil {
		detail += " · waiting " + prPipelineAge(*row.BlockedAt, now)
	}
	return detail
}

func boardBlockerEvidenceDetail(row telemetry.Blocked, now time.Time) string {
	for _, evidence := range row.BlockerEvidence {
		if !evidence.Unverifiable {
			continue
		}
		parts := []string{"unverifiable " + strings.ReplaceAll(strings.TrimSpace(evidence.Type), "_", " ")}
		if owner := strings.TrimSpace(evidence.Owner); owner != "" {
			parts = append(parts, "owner "+owner)
		}
		if evidence.RecordedAt != nil && !now.IsZero() {
			parts = append(parts, "age "+prPipelineAge(*evidence.RecordedAt, now))
		} else if evidence.AgeSeconds > 0 {
			parts = append(parts, "age "+formatDuration(float64(evidence.AgeSeconds)))
		}
		return strings.Join(parts, " · ")
	}
	return ""
}

func boardCardViewFromCard(data DashboardData, lane projectKanbanLane, card projectKanbanCard, terminal bool, scope string, fallbackProjectID string) boardCardView {
	// Legacy single-project snapshots can include issues without setting
	// Issue.ProjectID, so fall back to the scoped dashboard project so the
	// card slug and the sheet's project-scoped Move/Remove links resolve.
	projectID := strings.TrimSpace(card.ProjectID)
	if projectID == "" {
		projectID = fallbackProjectID
	}
	moveProjectID := strings.TrimSpace(projectKanbanCardProjectID(data, card))
	if moveProjectID == "" {
		moveProjectID = projectID
	}
	identity := boardCardIdentityToken(card.Identifier, card.IssueID, card.IssueNumber)
	moveDisabledText := projectKanbanCardMoveDisabledText(data, card)
	if card.RecentCompletion {
		moveDisabledText = ""
	}
	canDrag := moveDisabledText == "" && !card.RecentCompletion
	running := boardCardIsRunning(data.Snapshot, card)
	retrying := !running && boardCardIsRetrying(data.Snapshot, card)
	waiting := strings.EqualFold(lane.Title, "In Progress") && !running && !retrying
	view := boardCardView{
		DomID:             "card-" + boardCardScopedSlug(projectID, identity),
		Identity:          identity,
		IssueID:           card.IssueID,
		Number:            card.IssueNumber,
		URL:               card.URL,
		Project:           projectID,
		MoveProject:       moveProjectID,
		Scope:             scope,
		CurrentState:      card.Stage,
		DataSeq:           kanbanstate.SnapshotProjectDataSeq(data.Snapshot, moveProjectID),
		DragDrop:          canDrag || moveDisabledText != "",
		CanDrag:           canDrag,
		MoveDisabledText:  moveDisabledText,
		MoveDisabledLabel: boardMoveDisabledLabel(moveDisabledText),
		Running:           running,
		Retrying:          retrying,
		Waiting:           waiting,
		Done:              strings.EqualFold(lane.Title, "Done"),
		Terminal:          terminal,
		Title:             card.Title,
		State:             card.Stage,
		Origin:            card.Origin,
		Labels:            append([]string(nil), card.Labels...),
		Effort:            strings.TrimSpace(card.RuntimeIdentity.ReasoningEffort.Value),
	}
	if canDrag {
		view.AllowedTargets = projectKanbanMoveTargetKeys(data, card)
	}
	if card.PRNumber > 0 {
		view.PRNumber = strconv.Itoa(card.PRNumber)
		view.PRURL = card.PRURL
		view.MetaRight = "PR #" + strconv.Itoa(card.PRNumber)
	}
	if boardLaneShowsAge(lane.Title, view.Terminal) {
		view.AgeFooter = boardCompactAge(card.TimeInStage)
		view.AgeFooterTitle = strings.TrimSpace(card.TimeInStageTitle)
	}
	view.ExtraKind, view.ExtraText, view.ExtraChip = boardCardExtra(card, view)
	if stranded, ok := boardCardStrandedActiveIssue(data.Snapshot, card); ok {
		view.ExtraKind = primitives.KindWarn
		view.ExtraText = "Stranded " + boardCardStrandedAge(stranded.DurationSeconds) + " · no worker"
		view.ExtraChip = true
	}
	view.BlockerSummary = card.BlockerSummary
	view.ParkSummary, view.ParkDetail = boardCardParkSummary(card.ParkSummary)
	view.ProgressSummary, view.ProgressDetail, view.ProgressKind = boardCardCompletionProgress(card.CompletionProgress)
	view.OriginDetail = boardCardOriginDetail(card.Origin, card.OriginActor)
	view.AuthorDetail = boardCardAuthorDetail(card.AuthorID, card.OriginActor)
	if card.BlockedReason == "" && card.BlockedRecoveryAction == "" && card.BlockedRecoveryReason == "" {
		view.Activity = boardCardActivity(data.Snapshot, card)
	}
	view.PRStatus, view.PRStatusClass = boardCardPRStatus(card)
	if view.Running {
		view.RuntimeBadge = true
		view.RuntimeSummary = runtimeIdentitySummary(card.RuntimeIdentity)
		view.RuntimeCozyText = runtimeIdentityBadgeSummary(card.RuntimeIdentity, false)
		view.RuntimeComfyText = runtimeIdentityBadgeSummary(card.RuntimeIdentity, true)
		if view.RuntimeCozyText == "" {
			view.RuntimeCozyText = "agent working"
		}
		if view.RuntimeComfyText == "" {
			view.RuntimeComfyText = "agent working"
		}
		providerSessionID, detentSessionID := boardRuntimeSessionIDs(data.Snapshot, card)
		view.RuntimeDetail = runtimeIdentityFlyoutDetail(card.RuntimeIdentity, providerSessionID, detentSessionID)
	}
	view.PriorityBadge, view.PriorityTitle, view.PriorityDetail, view.PriorityTop = boardCardPriority(card)
	view.MergeLaneStatus = card.MergeLaneStatus
	view.MergeLaneDetail = card.MergeLaneDetail
	view.MergeLaneKind = card.MergeLaneKind
	view.CompactSignal = boardCardCompactSignal(view)
	return view
}

func boardCardStrandedActiveIssue(snapshot telemetry.Snapshot, card projectKanbanCard) (telemetry.StrandedIssue, bool) {
	for _, issue := range snapshot.StrandedActiveIssues {
		if strings.TrimSpace(issue.ProjectID) != "" && strings.TrimSpace(card.ProjectID) != "" &&
			!strings.EqualFold(strings.TrimSpace(issue.ProjectID), strings.TrimSpace(card.ProjectID)) {
			continue
		}
		if strings.TrimSpace(issue.IssueID) != "" && strings.TrimSpace(card.IssueID) != "" &&
			strings.TrimSpace(issue.IssueID) == strings.TrimSpace(card.IssueID) {
			return issue, true
		}
		if strings.TrimSpace(issue.Identifier) != "" && strings.TrimSpace(card.Identifier) != "" &&
			strings.EqualFold(strings.TrimSpace(issue.Identifier), strings.TrimSpace(card.Identifier)) {
			return issue, true
		}
		if strings.TrimSpace(issue.IssueURL) != "" && strings.TrimSpace(card.URL) != "" &&
			strings.TrimSpace(issue.IssueURL) == strings.TrimSpace(card.URL) {
			return issue, true
		}
	}
	return telemetry.StrandedIssue{}, false
}

func boardCardStrandedAge(seconds int64) string {
	duration := time.Duration(seconds) * time.Second
	switch {
	case duration >= 24*time.Hour:
		return strconv.FormatInt(int64(duration/(24*time.Hour)), 10) + "d"
	case duration >= time.Hour:
		return strconv.FormatInt(int64(duration/time.Hour), 10) + "h"
	case duration >= time.Minute:
		return strconv.FormatInt(int64(duration/time.Minute), 10) + "m"
	default:
		return strconv.FormatInt(max(seconds, 0), 10) + "s"
	}
}

func boardCardParkSummary(summary telemetry.ParkSummary) (string, string) {
	if summary.AttemptCount == 0 && summary.ParkCount == 0 && summary.Tokens == (telemetry.ParkTokenTotals{}) {
		return "", ""
	}
	line := formatInt(summary.AttemptCount) + " attempts · " + formatInt(summary.ParkCount) + " parks · tokens " +
		fleetCompactTokens(summary.Tokens.InputTokens) + " input / " +
		fleetCompactTokens(summary.Tokens.CachedInputTokens) + " cached / " +
		fleetCompactTokens(summary.Tokens.OutputTokens) + " output / " +
		fleetCompactTokens(summary.Tokens.ReasoningOutputTokens) + " reasoning"
	details := []string{line}
	for _, cause := range summary.Causes {
		details = append(details, cause.Cause+": "+formatInt(cause.Count)+"; first "+cause.FirstAt.Format(time.RFC3339)+"; last "+cause.LastAt.Format(time.RFC3339))
	}
	return line, strings.Join(details, "\n")
}

func boardCardCompletionProgress(progress telemetry.CompletionProgress) (string, string, primitives.Kind) {
	if strings.TrimSpace(progress.Outcome) == telemetry.CompletionProgressOutcomeNoProgress {
		summary := "Last turn · no progress"
		if progress.ConsecutiveNoProgress > 0 && progress.NoProgressLimit > 0 {
			summary += " " + strconv.Itoa(progress.ConsecutiveNoProgress) + "/" + strconv.Itoa(progress.NoProgressLimit)
		}
		return summary, summary + "; reason: " + strings.TrimSpace(progress.Reason), primitives.KindWarn
	}
	labels := make([]string, 0, len(progress.Kinds))
	for _, kind := range progress.Kinds {
		label := strings.ReplaceAll(strings.TrimSpace(kind), "_", " ")
		if label != "" {
			labels = append(labels, label)
		}
	}
	if len(labels) > 0 {
		summary := "Last turn · " + strings.Join(labels, " + ")
		return summary, summary + "; reason: " + strings.TrimSpace(progress.Reason), primitives.KindInfo
	}
	return "", "", ""
}

func boardCardOriginDetail(origin string, actor string) string {
	origin = strings.ToLower(strings.TrimSpace(origin))
	actor = strings.TrimSpace(actor)
	if origin == "" {
		return ""
	}
	detail := "via " + origin
	if actor != "" {
		detail += " · @" + strings.TrimPrefix(actor, "@")
	}
	return detail
}

func boardCardAuthorDetail(author string, originActor string) string {
	author = strings.TrimPrefix(strings.TrimSpace(author), "@")
	originActor = strings.TrimPrefix(strings.TrimSpace(originActor), "@")
	if author == "" || strings.EqualFold(author, originActor) {
		return ""
	}
	return "@" + author
}

func boardCardCompactSignal(card boardCardView) string {
	switch {
	case card.MergeLaneStatus != "":
		return card.MergeLaneStatus
	case card.RuntimeBadge && card.ExtraText == "agent working":
		return card.RuntimeCozyText
	case card.ExtraText != "":
		return card.ExtraText
	case card.AgeFooter != "":
		return "In lane " + card.AgeFooter
	case card.RuntimeBadge:
		return card.RuntimeCozyText
	case card.Done:
		return "Done"
	default:
		return ""
	}
}

func boardCardActivity(snapshot telemetry.Snapshot, card projectKanbanCard) string {
	for _, running := range snapshot.Running {
		if issueIdentifier(running.Issue) != card.Identifier || !sheetSessionMatchesProject(running.ProjectID, card) {
			continue
		}
		if message := boardCardActivityPreview(running.LastMessage); message != "" {
			return message
		}
		return boardCardActivityPreview(running.LastEvent)
	}
	var latest *telemetry.IssueComment
	for i := range card.Comments {
		comment := &card.Comments[i]
		if strings.TrimSpace(comment.Body) == "" {
			continue
		}
		if latest == nil || boardCardCommentTime(*comment).After(boardCardCommentTime(*latest)) {
			latest = comment
		}
	}
	if latest == nil {
		return ""
	}
	return boardCardActivityPreview(latest.Body)
}

func boardCardCommentTime(comment telemetry.IssueComment) time.Time {
	if comment.UpdatedAt != nil {
		return comment.UpdatedAt.UTC()
	}
	if comment.CreatedAt != nil {
		return comment.CreatedAt.UTC()
	}
	return time.Time{}
}

func boardCardActivityPreview(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	const limit = 96
	if len([]rune(value)) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit-3]) + "..."
}

func boardCardPRStatus(card projectKanbanCard) (string, string) {
	if card.PRNumber <= 0 {
		return "", ""
	}
	status := "PR #" + strconv.Itoa(card.PRNumber)
	if ci := strings.TrimSpace(card.CIStatus); ci != "" {
		status += " · CI " + ci
	}
	return status, card.CIClass
}

func boardRuntimeSessionIDs(snapshot telemetry.Snapshot, card projectKanbanCard) (string, int64) {
	for _, running := range snapshot.Running {
		if issueIdentifier(running.Issue) == card.Identifier && sheetSessionMatchesProject(running.ProjectID, card) {
			return strings.TrimSpace(running.SessionID), running.DetentSessionID
		}
	}
	return "", 0
}

func boardCardPriority(card projectKanbanCard) (string, string, string, bool) {
	if card.PriorityRank == 0 && card.DispatchPriorityRank == 0 && card.UnblockerCount == 0 {
		return "", "", "", false
	}

	badge := strings.TrimSpace(card.PriorityName)
	if badge == "" && card.PriorityRank > 0 {
		badge = "rank " + strconv.Itoa(card.PriorityRank)
	}
	if badge == "" {
		badge = card.DispatchPriorityLabel
	}
	if badge == "" && card.UnblockerCount > 0 {
		badge = "unblocker"
	}
	details := make([]string, 0, 3)
	if card.PriorityRank > 0 {
		name := strings.TrimSpace(card.PriorityName)
		if name == "" {
			name = "Tracker priority"
		} else {
			name = "Tracker priority " + name
		}
		details = append(details, name+" maps to dispatch rank "+strconv.Itoa(card.PriorityRank)+".")
	}
	if card.DispatchPriorityRank > 0 {
		details = append(details, "Label "+card.DispatchPriorityLabel+" is configured at dispatch label rank "+strconv.Itoa(card.DispatchPriorityRank)+".")
	}
	if card.UnblockerCount > 0 {
		details = append(details, unblockerPriorityDetail(card.UnblockerCount))
	}
	top := card.PriorityRank == 1 || (card.PriorityRank == 0 && card.DispatchPriorityRank == 1)
	return badge, "Dispatch priority", strings.Join(details, " "), top
}

func unblockerPriorityDetail(count int) string {
	if count == 1 {
		return "Unblocks 1 issue."
	}
	return "Unblocks " + strconv.Itoa(count) + " issues."
}

// boardCardExtra picks the single allowed extra signal, most urgent first:
// an exception chip, then a status line. Cards never stack signals.
func boardCardExtra(card projectKanbanCard, view boardCardView) (primitives.Kind, string, bool) {
	if view.Done || view.Terminal {
		return primitives.KindNeutral, "", false
	}
	if boardBlockedWaiting(card.BlockedSource, card.BlockedRecoveryAction, card.BlockedRecoveryReason, card.BlockedReason) {
		return primitives.KindWarn, boardCardBlockedWaitingText(card), true
	}
	if reason := boardBlockedDetail(card.BlockedSource, card.BlockedRecoveryAction, card.BlockedRecoveryReason, card.BlockedRecoveryRemedy, card.BlockedReason); reason != "" {
		return primitives.KindErr, "needs review - " + reason, true
	}
	if label := strings.TrimSpace(card.AttentionLabel); label != "" {
		return primitives.KindErr, "blocked — " + label, true
	}
	if len(card.Blockers) > 0 {
		return primitives.KindErr, "blocked — " + card.Blockers[0], true
	}
	if reason := strings.TrimSpace(card.ConflictReason); reason != "" {
		return primitives.KindWarn, reason, true
	}
	if card.GatePending {
		return primitives.KindInfo, "Awaiting checks", true
	}
	if detail := strings.TrimSpace(card.WaitDetail); detail != "" {
		return primitives.KindInfo, detail, false
	}
	if view.Retrying {
		return primitives.KindInfo, "Awaiting retry", true
	}
	if view.Waiting {
		return primitives.KindNeutral, "No live attempt", true
	}
	if status := strings.TrimSpace(card.CIStatus); status != "" {
		return primitives.KindInfo, status, false
	}
	if view.Running {
		return primitives.KindOK, "agent working", false
	}
	return primitives.KindNeutral, "", false
}

func boardCardIsRunning(snapshot telemetry.Snapshot, card projectKanbanCard) bool {
	for _, running := range snapshot.Running {
		if boardCardMatchesIssue(running.Issue, card) {
			return true
		}
	}
	return false
}

func boardCardIsRetrying(snapshot telemetry.Snapshot, card projectKanbanCard) bool {
	for _, retry := range snapshot.Queue {
		if boardCardMatchesIssue(retry.Issue, card) {
			return true
		}
	}
	return false
}

func boardCardMatchesIssue(issue telemetry.Issue, card projectKanbanCard) bool {
	if strings.TrimSpace(issue.ID) != "" && strings.TrimSpace(card.IssueID) != "" && strings.TrimSpace(issue.ID) == strings.TrimSpace(card.IssueID) {
		return sheetSessionMatchesProject(issue.ProjectID, card)
	}
	return issueIdentifier(issue) == card.Identifier && sheetSessionMatchesProject(issue.ProjectID, card)
}

func boardCardBlockedWaitingText(card projectKanbanCard) string {
	if len(card.Blockers) > 0 {
		return "waiting on " + card.Blockers[0]
	}
	if len(card.ClearedBlockers) > 0 && boardBlockedDependencyWaiting(card.BlockedSource, card.BlockedRecoveryReason, card.BlockedReason, nil) {
		return "waiting - project status"
	}
	if detail := boardBlockedDetail(card.BlockedSource, card.BlockedRecoveryAction, card.BlockedRecoveryReason, card.BlockedRecoveryRemedy, card.BlockedReason); detail != "" {
		return "waiting - " + detail
	}
	if boardBlockedDependencyWaiting(card.BlockedSource, card.BlockedRecoveryReason, card.BlockedReason, nil) {
		return "waiting - dependency"
	}
	return "waiting - project status"
}

func boardBlockedWaiting(source telemetry.BlockedSource, recoveryAction string, recoveryReason string, reason string) bool {
	switch strings.ToLower(strings.TrimSpace(recoveryAction)) {
	case "hold":
		return false
	case "defer":
		return true
	}
	if source == telemetry.BlockedSourceOperatorStop && !operatorStopTransitionFailed(telemetry.Blocked{Source: source, RecoveryReason: recoveryReason, Error: reason}) {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(recoveryReason), "human_blocker") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(recoveryReason), "dependency_blocker") {
		return true
	}
	switch telemetry.BlockedSource(strings.TrimSpace(string(source))) {
	case telemetry.BlockedSourceDependency, telemetry.BlockedSourceProjectStatus:
		return true
	default:
		return boardBlockedLegacyWaitingReason(reason)
	}
}

func boardBlockedDependencyWaiting(source telemetry.BlockedSource, recoveryReason string, reason string, blockers []telemetry.BlockedRef) bool {
	if telemetry.BlockedSource(strings.TrimSpace(string(source))) == telemetry.BlockedSourceDependency {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(recoveryReason), "dependency_blocker") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(reason), "blocked by non-terminal dependency") {
		return true
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(reason)), "depends on ") {
		return true
	}
	return len(blockers) > 0
}

func boardBlockedLegacyWaitingReason(reason string) bool {
	reason = strings.TrimSpace(reason)
	if strings.EqualFold(reason, "blocked by non-terminal dependency") {
		return true
	}
	if strings.EqualFold(reason, "blocked by project status") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(reason), "depends on ")
}

func boardBlockedDetail(source telemetry.BlockedSource, recoveryAction string, recoveryReason string, recoveryRemedy string, reason string) string {
	if strings.EqualFold(strings.TrimSpace(recoveryAction), "hold") {
		detail := strings.TrimSpace(reason)
		if detail == "" {
			detail = strings.ReplaceAll(strings.TrimSpace(recoveryReason), "_", " ")
		}
		if detail == "" {
			detail = "blocked recovery held"
		}
		if remedy := strings.TrimSpace(recoveryRemedy); remedy != "" {
			detail += " — " + remedy
		}
		return detail
	}
	reason = strings.TrimSpace(reason)
	switch reason {
	case "blocked by non-terminal dependency":
		return "dependency not ready"
	case "blocked by project status":
		if strings.EqualFold(strings.TrimSpace(recoveryReason), "dependency_blocker") ||
			telemetry.BlockedSource(strings.TrimSpace(string(source))) == telemetry.BlockedSourceDependency {
			return "dependency not ready"
		}
		return "paused by project status"
	default:
		return reason
	}
}

// boardFirstRun is true only when nothing is configured at all: no
// projects registered and no usable board data. Running mode always has
// at least one project, so this is effectively the unconfigured guard.
func boardFirstRun(data DashboardData) bool {
	return len(data.Projects) == 0 && !projectKanbanBoardLoaded(data)
}

func BoardShellDataFromDashboard(data DashboardData) DashboardShellData {
	shell := DashboardShellDataFromDashboard(data)
	shell.ActiveNav = "board"
	shell.IncludeDashboardCharts = false
	return shell
}

func boardBoolAttr(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func boardLaneCountLabel(view boardView) string {
	return formatCount(view.Visible) + "/" + formatCount(view.Total)
}

func boardLaneHiddenCardBadgeLabel(view boardView) string {
	if view.HiddenCards <= 0 {
		return ""
	}
	return formatCount(view.HiddenCards) + " hidden"
}

func boardLaneHiddenCardSummary(view boardView) string {
	hidden := make([]boardLaneView, 0)
	for _, lane := range view.Lanes {
		if boardLaneHiddenPopulated(lane) {
			hidden = append(hidden, lane)
		}
	}
	return boardLaneHiddenLaneSummary(hidden)
}

func boardLaneHiddenLaneSummary(lanes []boardLaneView) string {
	if len(lanes) == 0 {
		return "All populated lanes are visible."
	}
	if len(lanes) == 1 {
		lane := lanes[0]
		return boardCountLabel(lane.CardCount, "hidden card", "hidden cards") + " in " + lane.Title + "."
	}
	total := 0
	parts := make([]string, 0, len(lanes))
	for _, lane := range lanes {
		total += lane.CardCount
		parts = append(parts, lane.Title+" ("+formatCount(lane.CardCount)+")")
	}
	return boardCountLabel(total, "hidden card", "hidden cards") + " across " + boardCountLabel(len(lanes), "lane", "lanes") + ": " + strings.Join(parts, ", ") + "."
}

func boardLaneHiddenPopulated(lane boardLaneView) bool {
	return lane.CardCount > 0 && !lane.DefaultVisible
}

func boardLaneVisibilityStatusLabel(lane boardLaneView) string {
	if lane.DefaultVisible {
		return "Auto shown"
	}
	if lane.CardCount > 0 {
		return "Auto hidden - " + boardCountLabel(lane.CardCount, "hidden card", "hidden cards")
	}
	return "Auto hidden"
}

func boardLaneVisibilityStatusTitle(lane boardLaneView) string {
	if lane.DefaultVisible {
		return "Auto follows the board default; this lane is currently shown."
	}
	if lane.CardCount > 0 {
		return "Auto follows the board default; " + boardCountLabel(lane.CardCount, "card is", "cards are") + " currently hidden."
	}
	return "Auto follows the board default; this lane is currently hidden."
}

func boardLaneVisibilityRowClass(lane boardLaneView) string {
	class := "grid gap-1 rounded-card border px-2 py-1.5 text-xs text-text hover:bg-surface"
	if boardLaneHiddenPopulated(lane) {
		return class + " border-warn/45 bg-warn/10"
	}
	return class + " border-transparent"
}

func boardCountLabel(count int, singular string, plural string) string {
	if count == 1 {
		return "1 " + singular
	}
	return formatCount(count) + " " + plural
}

func boardScopeLabel(data DashboardData) string {
	if name := strings.TrimSpace(data.ProjectName); name != "" {
		return name
	}
	if id := strings.TrimSpace(data.ProjectID); id != "" {
		return id
	}
	return "All projects"
}

func boardFeedbackClass(kind string) string {
	if kind == "error" {
		return "text-err"
	}
	return "text-sec"
}

func boardFeedbackGlyph(kind string) string {
	if kind == "error" {
		return "⬣"
	}
	return "✓"
}

func boardCardClass(card boardCardView) string {
	var base string
	if card.Done {
		base = "flex flex-none flex-col gap-1.5 rounded-card border border-line bg-surface p-3 opacity-75"
	} else if card.ExtraChip && card.ExtraKind == primitives.KindWarn {
		base = "flex flex-none flex-col gap-1.5 rounded-card border border-warn/45 bg-elev p-3"
	} else if card.ExtraChip && card.ExtraKind == primitives.KindErr {
		base = "flex flex-none flex-col gap-1.5 rounded-card border border-err/45 bg-elev p-3"
	} else {
		base = "flex flex-none flex-col gap-1.5 rounded-card border border-line bg-elev p-3"
	}
	if card.PriorityTop {
		return base + " border-l-4 border-l-err"
	}
	if card.PriorityBadge != "" {
		return base + " border-l-2 border-l-warn"
	}
	return base
}

func boardLanesClass(data DashboardData) string {
	base := "dt-lane-scroll flex min-h-0 min-w-0 flex-1 snap-x snap-mandatory gap-5 overflow-x-auto overflow-y-hidden scroll-px-5 px-5 pb-5 md:snap-none md:gap-3"
	if data.Snapshot.LastKnown && !snapshotUsesStartupCache(data.Snapshot) {
		return base + " opacity-60 grayscale"
	}
	return base
}

func boardPriorityBadgeClass(card boardCardView) string {
	base := "inline-flex min-w-7 max-w-24 shrink items-center rounded-chip border px-1.5 py-0.5 font-mono text-2xs font-semibold"
	if card.PriorityTop {
		return base + " border-err/30 bg-err/15 text-err"
	}
	return base + " border-warn/30 bg-warn/15 text-warn"
}

func boardCardInteractionClass(card boardCardView) string {
	base := " select-none data-[kanban-dragging=true]:opacity-60 data-[kanban-connection-disabled=true]:cursor-not-allowed data-[kanban-connection-disabled=true]:opacity-60 data-[kanban-connection-disabled=true]:hover:border-line"
	if card.CanDrag {
		return base + " cursor-grab hover:border-accent/50 active:cursor-grabbing"
	}
	if card.MoveDisabledText != "" {
		return base + " cursor-pointer hover:border-line"
	}
	return " cursor-pointer hover:border-accent/50"
}

func boardMoveDisabledLabel(reason string) string {
	reason = strings.ToLower(strings.TrimSpace(reason))
	switch {
	case reason == "":
		return ""
	case strings.Contains(reason, "initializing"):
		return "Initializing"
	case strings.Contains(reason, "all-project"), strings.Contains(reason, "read-only"), strings.Contains(reason, "tracker does not support"):
		return "Read-only"
	case strings.Contains(reason, "snapshot"), strings.Contains(reason, "refresh"):
		return "Stale"
	case strings.Contains(reason, "linked issue"):
		return "No issue"
	default:
		return "No move"
	}
}

func boardCardNumberClass(card boardCardView) string {
	if card.Done {
		return "flex-none max-w-16 truncate text-sec"
	}
	return "flex-none max-w-16 truncate text-text"
}

func boardCardTitleClass(card boardCardView) string {
	if card.Done {
		return "line-clamp-2 text-sm text-sec"
	}
	return "line-clamp-2 text-sm text-text"
}

func boardExtraTextClass(kind primitives.Kind) string {
	switch kind {
	case primitives.KindOK:
		return "text-ok"
	case primitives.KindWarn:
		return "text-warn"
	case primitives.KindErr:
		return "text-err"
	case primitives.KindInfo:
		return "text-info"
	}
	return "text-sec"
}

// boardCompactAge reduces "3m 39s" to "3m": board cards are narrow, and
// the leading unit is all an at-a-glance read needs. Numbers must never
// wrap or clip, so the value is shortened rather than truncated.
func boardCompactAge(age string) string {
	age = strings.TrimSpace(age)
	if age == "" || age == "n/a" {
		return ""
	}
	if head, _, ok := strings.Cut(age, " "); ok {
		return head
	}
	return age
}

// boardLaneShowsAge keeps intake and terminal lanes quiet: time-in-stage only
// matters once work is moving and before it has finished.
func boardLaneShowsAge(title string, terminal bool) bool {
	if terminal {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(title)) {
	case "backlog", "todo", "done", "cancelled", "canceled", "closed", "duplicate":
		return false
	}
	return true
}

func boardCardIdentityToken(identifier string, issueID string, number string) string {
	identifier = strings.TrimSpace(identifier)
	if issueIdentifierHasNumber(identifier) {
		return identifier
	}
	if issueID = strings.TrimSpace(issueID); issueID != "" {
		return issueID
	}
	return strings.TrimSpace(number)
}

func issueIdentifierHasNumber(identifier string) bool {
	index := strings.LastIndex(identifier, "#")
	return index > 0 && index < len(identifier)-1
}

func issueIdentifierHasRepositoryNumber(identifier string) bool {
	index := strings.LastIndex(identifier, "#")
	return index > 0 && index < len(identifier)-1 && strings.Contains(identifier[:index], "/")
}

func boardCardScopedIdentityToken(projectID string, identity string) string {
	projectID = strings.TrimSpace(projectID)
	identity = strings.TrimSpace(identity)
	if identity == "" || projectID == "" || issueIdentifierHasRepositoryNumber(identity) {
		return identity
	}
	return projectID + ":" + identity
}

func boardCardScopedSlug(projectID string, identity string) string {
	return boardCardSlug(boardCardScopedIdentityToken(projectID, identity))
}

func boardCardSlug(identity string) string {
	var builder strings.Builder
	lastDash := false
	for _, r := range strings.TrimSpace(identity) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			builder.WriteRune(r)
			lastDash = false
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r + ('a' - 'A'))
			lastDash = false
		default:
			if builder.Len() > 0 && !lastDash {
				builder.WriteByte('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if slug == "" {
		return "unknown"
	}
	return slug
}
