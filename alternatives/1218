package hubserver

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func readRecoveryChangeCheck(ctx context.Context, q nativeQueryer, change, version, run string) ([]tracker.ChangeCheck, error) {
	rows, err := q.QueryContext(ctx, `SELECT record_json,check_run_id FROM change_evidence WHERE change_id=? AND kind='check' AND version_id=? AND (check_run_id=? OR check_run_id IS NULL) ORDER BY sequence DESC LIMIT ?`, change, version, run, changeLegacyReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	checks := []tracker.ChangeCheck{}
	examined := 0
	for rows.Next() {
		if examined == changeLegacyReadLimit {
			return nil, errChangeColumnsIncomplete
		}
		examined++
		var raw string
		var stored sql.NullString
		if err := rows.Scan(&raw, &stored); err != nil {
			return nil, err
		}
		var check tracker.ChangeCheck
		if err := decodeStorageJSON(ctx, []byte(raw), &check); err != nil {
			return nil, err
		}
		if !stored.Valid && check.CheckRunID != run {
			continue
		}
		checks = append(checks, check)
		break
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i := range checks {
		if err := hydrateChangeRecord(ctx, q, &checks[i]); err != nil {
			return nil, err
		}
	}
	return checks, nil
}

func legacyRecoveryReviews(ctx context.Context, q nativeQueryer, change, version string) ([]tracker.ChangeReview, map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT sequence,record_json FROM change_evidence WHERE change_id=? AND version_id=? AND kind='review' ORDER BY sequence DESC LIMIT ?`, change, version, changeLegacyReadLimit+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	type selectedReview struct {
		sequence int64
		review   tracker.ChangeReview
	}
	selected := []selectedReview{}
	roles := map[string]bool{}
	actors := map[string]bool{}
	decisions := map[string]bool{}
	count := 0
	for rows.Next() {
		if count == changeLegacyReadLimit {
			return nil, nil, errChangeColumnsIncomplete
		}
		var sequence int64
		var raw string
		if err := rows.Scan(&sequence, &raw); err != nil {
			return nil, nil, err
		}
		var review tracker.ChangeReview
		if err := decodeStorageJSON(ctx, []byte(raw), &review); err != nil {
			return nil, nil, err
		}
		keep := count < 32
		count++
		kinds := []string{}
		if review.ImplementationReview != nil {
			kinds = append(kinds, "implementation")
		}
		if review.Validator != nil {
			kinds = append(kinds, "validator")
		}
		if audit := review.SecurityAudit; audit != nil {
			kinds = append(kinds, "audit")
			if !audit.Landing {
				kinds = append(kinds, "source-audit")
			}
			if review.Decision == "changes_requested" {
				kinds = append(kinds, "audit-refusal")
			}
		}
		for _, kind := range kinds {
			if !roles[kind] {
				roles[kind] = true
				keep = true
			}
		}
		if len(kinds) == 0 && review.Decision != "commented" && !actors[review.Actor.PrincipalID] {
			actors[review.Actor.PrincipalID] = true
			decisions[review.Decision] = true
		}
		if keep {
			selected = append(selected, selectedReview{sequence: sequence, review: review})
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	slices.SortFunc(selected, func(a, b selectedReview) int { return cmp.Compare(a.sequence, b.sequence) })
	reviews := make([]tracker.ChangeReview, 0, len(selected))
	for _, entry := range selected {
		review := entry.review
		if err := hydrateChangeRecord(ctx, q, &review); err != nil {
			return nil, nil, err
		}
		reviews = append(reviews, review)
	}
	return reviews, decisions, nil
}
