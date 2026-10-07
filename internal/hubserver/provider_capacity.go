package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func providerWait(code, message string) error {
	return &nativeError{Code: code, Message: message, status: http.StatusConflict}
}

func isProviderWait(err error) bool {
	var failure *nativeError
	return errors.As(err, &failure) && failure != nil && (failure.Code == "provider_capacity" || failure.Code == "provider_incompatible" || failure.Code == "provider_candidate_changed")
}

func validateProviderCandidates(candidates []tracker.NativeCapacityCandidate) error {
	if len(candidates) > 100 {
		return nativeInvalid("Provider selection is limited to 100 candidates per page")
	}
	seen := make(map[tracker.NativeWorkItemID]bool)
	for _, candidate := range candidates {
		if candidate.WorkItemID == "" || candidate.Revision <= 0 || seen[candidate.WorkItemID] {
			return nativeInvalid("Provider candidates require distinct work items and positive revisions")
		}
		if err := candidate.Requirement.Validate(); err != nil {
			return nativeInvalid(err.Error())
		}
		seen[candidate.WorkItemID] = true
	}
	return nil
}

func updateProviderReports(ctx context.Context, tx *sql.Tx, scope nativeScope, reports []providercapacity.Report, now time.Time) error {
	if reports == nil {
		return nil
	}
	if err := providercapacity.Validate(reports); err != nil {
		return nativeInvalid(err.Error())
	}
	if len(reports) == 0 {
		return nativeInvalid("An enabled provider reporter must retain at least one backend")
	}
	previous, err := readProviderReports(ctx, tx, scope.credential.Runner.RunnerID)
	if err != nil {
		return err
	}
	for i, report := range reports {
		for _, prior := range previous {
			if prior.Backend != report.Backend || prior.Provider != report.Provider || prior.AccountAlias != report.AccountAlias || prior.SharedAccountAlias != report.SharedAccountAlias || prior.ObservedAt.After(now) {
				continue
			}
			if report.ObservedAt.After(now) || prior.ObservedAt.After(report.ObservedAt) || prior.ObservedAt.Equal(report.ObservedAt) && prior.Availability == "exhausted" && report.Availability != "exhausted" {
				reports[i] = prior
			}
		}
	}
	raw, err := json.Marshal(reports)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE runner_identities SET provider_reports_json = ? WHERE id = ? AND organization_id = ?", string(raw), scope.credential.Runner.RunnerID, scope.organization)
	return err
}

func readProviderReports(ctx context.Context, query nativeQueryer, runner string) ([]providercapacity.Report, error) {
	var raw string
	if err := query.QueryRowContext(ctx, "SELECT provider_reports_json FROM runner_identities WHERE id = ?", runner).Scan(&raw); err != nil {
		return nil, err
	}
	var reports []providercapacity.Report
	err := json.Unmarshal([]byte(raw), &reports)
	return reports, err
}

func sharedProviderAccount(a, b providercapacity.Report) bool {
	return a.Provider == b.Provider && (a.SharedAccountAlias == "" || b.SharedAccountAlias == "" || a.SharedAccountAlias == b.SharedAccountAlias)
}

type providerCapacitySnapshot struct {
	groups       map[string]providercapacity.Report
	reports      []providercapacity.Report
	reservations []providercapacity.Reservation
}

func readProviderCapacitySnapshot(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, now time.Time) (providerCapacitySnapshot, error) {
	var snapshot providerCapacitySnapshot
	rows, err := query.QueryContext(ctx, `SELECT r.provider_reports_json, t.created_at, t.expires_at, t.revoked_at
FROM runner_identities r JOIN api_tokens t ON t.id = r.token_id WHERE r.organization_id = ?`, organization)
	if err != nil {
		return snapshot, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw, created string
		var expires, revoked sql.NullString
		var reports []providercapacity.Report
		if err := rows.Scan(&raw, &created, &expires, &revoked); err != nil {
			return snapshot, err
		}
		if revoked.Valid || !runnerTimeValid(now, created, expires.String) {
			continue
		}
		if err := json.Unmarshal([]byte(raw), &reports); err != nil {
			return snapshot, err
		}
		snapshot.reports = append(snapshot.reports, reports...)
	}
	if err := rows.Err(); err != nil {
		return snapshot, err
	}
	if err := rows.Close(); err != nil {
		return snapshot, err
	}
	rows, err = query.QueryContext(ctx, `SELECT p.reservation_json, l.expires_at, l.session_id, l.machine_id, `+nativeLandingCapacityProject+` FROM provider_reservations p JOIN leases l ON l.lease_id=p.lease_id JOIN issues i ON i.id=l.issue_id `+nativeLandingCapacityJoins+` WHERE p.organization_id = ? AND l.released_at IS NULL`, organization)
	if err != nil {
		return snapshot, err
	}
	defer rows.Close()
	snapshot.groups = map[string]providercapacity.Report{}
	for rows.Next() {
		var raw, expiry, session, project string
		var machine tracker.MachineID
		var reservation providercapacity.Reservation
		if err := rows.Scan(&raw, &expiry, &session, &machine, &project); err != nil {
			return snapshot, err
		}
		end, err := parseTimeValue(expiry)
		if err != nil {
			return snapshot, err
		}
		if !end.After(now) {
			continue
		}
		if err := json.Unmarshal([]byte(raw), &reservation); err != nil {
			return snapshot, err
		}
		if reservation.Backend == "git" && reservation.Role == "merge" {
			group := landingCapacityKey(machine, project, session)
			if group != "" {
				if _, exists := snapshot.groups[group]; exists {
					continue
				}
				snapshot.groups[group] = reservation.Report
			}
		}
		snapshot.reservations = append(snapshot.reservations, reservation)
	}
	return snapshot, rows.Err()
}

func providerView(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, report providercapacity.Report, now time.Time) (providercapacity.View, error) {
	snapshot, err := readProviderCapacitySnapshot(ctx, query, organization, now)
	if err != nil {
		return providercapacity.View{}, err
	}
	return snapshot.view(report, now), nil
}

func (snapshot providerCapacitySnapshot) view(report providercapacity.Report, now time.Time) providercapacity.View {
	view := providercapacity.View{Report: report, State: report.State(now), Reason: "Bounded concurrency available; quota is an observation, not transferable credit"}
	var freshReports []providercapacity.Report
	for _, other := range snapshot.reports {
		if !now.Before(other.ObservedAt) && now.Before(other.ObservedAt.Add(providercapacity.MaxAge)) {
			freshReports = append(freshReports, other)
		}
		if !sharedProviderAccount(report, other) {
			continue
		}
		view.MaxConcurrent = min(view.MaxConcurrent, other.MaxConcurrent)
		if other.State(now) == "exhausted" {
			view.State = "exhausted"
			if other.ResetAt.After(view.ResetAt) {
				view.ResetAt = other.ResetAt
			}
		} else if other.State(now) == "unknown" && view.State != "exhausted" {
			view.State = "unknown"
		}
	}
	for _, reservation := range snapshot.reservations {
		if sharedProviderAccount(report, reservation.Report) {
			view.Used++
			if !slices.ContainsFunc(freshReports, func(current providercapacity.Report) bool {
				return current.Provider == reservation.Report.Provider && current.Backend == reservation.Report.Backend && current.AccountAlias == reservation.Report.AccountAlias && current.SharedAccountAlias == reservation.Report.SharedAccountAlias && current.Supports(reservation.Requirement)
			}) {
				view.MaxConcurrent = min(view.MaxConcurrent, reservation.Report.MaxConcurrent)
			}
		}
	}
	switch {
	case view.State == "exhausted":
		view.Reason = "Provider account is exhausted; wait for a fresh observation or reset hint"
	case view.Used >= view.MaxConcurrent:
		view.Reason = "Shared provider concurrency is fully reserved; wait for lease release or expiry"
	case view.State == "unknown":
		view.Reason = "Quota is unknown or stale; only the declared concurrency bound is available"
	}
	return view
}

func selectProviderCapacity(ctx context.Context, tx *sql.Tx, query claimCandidateQuery, id tracker.WorkItemID, now time.Time) (providercapacity.Reservation, bool, error) {
	if query.NativeScope == nil || query.NativeScope.credential.Runner.RunnerID == "" {
		if len(query.ProviderCandidates) != 0 {
			return providercapacity.Reservation{}, false, nativeInvalid("Provider selection requires an enrolled runner")
		}
		return providercapacity.Reservation{}, false, nil
	}
	reports, err := readProviderReports(ctx, tx, query.NativeScope.credential.Runner.RunnerID)
	if err != nil {
		return providercapacity.Reservation{}, false, err
	}
	if len(reports) == 0 && len(query.ProviderCandidates) == 0 {
		return providercapacity.Reservation{}, false, nil
	}
	var native tracker.NativeWorkItemID
	var revision tracker.Revision
	if err := tx.QueryRowContext(ctx, "SELECT native_id, revision FROM issues WHERE id = ?", id).Scan(&native, &revision); err != nil {
		return providercapacity.Reservation{}, false, err
	}
	for _, candidate := range query.ProviderCandidates {
		if candidate.WorkItemID != native {
			continue
		}
		if candidate.Revision != revision {
			return providercapacity.Reservation{}, false, providerWait("provider_candidate_changed", "Work item changed after local model selection; refresh before claiming")
		}
		for _, report := range reports {
			if !report.Supports(candidate.Requirement) {
				continue
			}
			snapshot, err := readProviderCapacitySnapshot(ctx, tx, query.NativeScope.organization, now)
			if err != nil {
				return providercapacity.Reservation{}, false, err
			}
			view := snapshot.view(report, now)
			if group, exists := snapshot.groups[query.LandingBatchGroup]; exists && candidate.Requirement.Backend == "git" && candidate.Requirement.Role == "merge" && sharedProviderAccount(report, group) {
				view.Used--
			}
			if view.State == "exhausted" || view.Used >= view.MaxConcurrent {
				return providercapacity.Reservation{}, false, providerWait("provider_capacity", view.Reason)
			}
			return providercapacity.Reservation{Requirement: candidate.Requirement, Report: report, Reason: view.Reason}, true, nil
		}
		return providercapacity.Reservation{}, false, providerWait("provider_incompatible", "Runner does not advertise the selected backend and model; model and host requirements remain fixed")
	}
	if len(query.ProviderCandidates) != 0 {
		return providercapacity.Reservation{}, false, ErrNoClaimableWork
	}
	return providercapacity.Reservation{}, false, providerWait("provider_incompatible", "Work item needs local model selection before a provider reservation can be claimed")
}

func writeProviderReservation(ctx context.Context, tx *sql.Tx, lease tracker.LeaseID, organization tracker.OrganizationID, reservation providercapacity.Reservation) error {
	raw, err := json.Marshal(reservation)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO provider_reservations (lease_id, organization_id, pool, reservation_json) VALUES (?, ?, ?, ?)", lease, organization, reservation.Report.Pool(), string(raw))
	return err
}

func readProviderReservation(ctx context.Context, query nativeQueryer, lease tracker.LeaseID) (providercapacity.Reservation, bool, error) {
	var raw string
	if err := query.QueryRowContext(ctx, "SELECT reservation_json FROM provider_reservations WHERE lease_id = ?", lease).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return providercapacity.Reservation{}, false, nil
		}
		return providercapacity.Reservation{}, false, err
	}
	var reservation providercapacity.Reservation
	err := json.Unmarshal([]byte(raw), &reservation)
	return reservation, err == nil, err
}

func validateProviderAttempt(ctx context.Context, tx *sql.Tx, scope nativeScope, data tracker.NativeRunData, now time.Time) error {
	reservation, found, err := readProviderReservation(ctx, tx, data.LeaseID)
	if err != nil || !found {
		return err
	}
	if data.Identity == nil || reservation.Requirement != (providercapacity.Requirement{Role: data.Identity.Role, Backend: data.Identity.Backend, Model: data.Identity.Model}) {
		return providerWait("provider_incompatible", "Execution identity must match the reserved role, backend and model")
	}
	reports, err := readProviderReports(ctx, tx, scope.credential.Runner.RunnerID)
	if err != nil {
		return err
	}
	for _, report := range reports {
		if !report.Supports(reservation.Requirement) || report.Provider != reservation.Report.Provider || report.AccountAlias != reservation.Report.AccountAlias || report.SharedAccountAlias != reservation.Report.SharedAccountAlias {
			continue
		}
		view, err := providerView(ctx, tx, scope.organization, report, now)
		if err != nil {
			return err
		}
		if view.State == "exhausted" || view.Used > view.MaxConcurrent {
			return providerWait("provider_capacity", "Provider capacity changed after reservation; release the lease and wait")
		}
		return nil
	}
	return providerWait("provider_incompatible", "Provider account or capability changed after reservation")
}
