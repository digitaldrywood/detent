package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func checkDoctorBudgetOverrides(ctx context.Context, resolution globalconfig.PathResolution, projectID string, now time.Time, deps doctorDeps) doctorCheck {
	check := doctorCheck{Name: "Budget overrides", Status: doctorOK, Detail: "no active temporary overrides"}
	path := strings.TrimSpace(resolution.Path)
	if path == "" {
		check.Status = doctorWarn
		check.Detail = "skipped because the global config path is unavailable"
		return check
	}
	deps = deps.withDefaults()
	db, err := deps.openSQLiteReadOnly(ctx, filepath.Join(filepath.Dir(path), "detent.db"))
	if err != nil {
		if errors.Is(err, errDoctorTelemetryStoreUnavailable) {
			check.Detail = "no runtime database yet"
			return check
		}
		check.Status = doctorWarn
		check.Detail = fmt.Sprintf("could not read runtime overrides: %v", err)
		return check
	}
	rows, err := db.QueryContext(ctx, `
SELECT project_id, per_day_max_usd, per_issue_max_usd, expires_at, reason
FROM budget_overrides
WHERE expires_at > ?
ORDER BY expires_at, project_id`, now.UTC().Format(time.RFC3339))
	if err != nil {
		closeErr := db.Close()
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			check.Detail = "no active temporary overrides"
			if closeErr != nil {
				check.Status = doctorWarn
				check.Detail = "close runtime override store: " + closeErr.Error()
			}
			return check
		}
		check.Status = doctorWarn
		check.Detail = fmt.Sprintf("could not list runtime overrides: %v", err)
		if closeErr != nil {
			check.Detail += "; close runtime store: " + closeErr.Error()
		}
		return check
	}
	defer rows.Close()
	details, scanErr := doctorBudgetOverrideDetails(rows, strings.TrimSpace(projectID), now)
	closeDBErr := db.Close()
	if scanErr != nil || closeDBErr != nil {
		check.Status = doctorWarn
		check.Detail = fmt.Sprintf("read runtime override query: %v", errors.Join(scanErr, closeDBErr))
		return check
	}
	if len(details) > 0 {
		check.Status = doctorWarn
		check.Detail = strings.Join(details, "; ")
		check.Hint = "Review temporary budget access and clear it early with detent budget override clear when it is no longer needed."
	}
	return check
}

func doctorBudgetOverrideDetails(rows *sql.Rows, selectedProjectID string, now time.Time) ([]string, error) {
	details := []string{}
	for rows.Next() {
		var projectID string
		var dayCap sql.NullFloat64
		var issueCap sql.NullFloat64
		var expiresAtText string
		var reason string
		if err := rows.Scan(&projectID, &dayCap, &issueCap, &expiresAtText, &reason); err != nil {
			return nil, err
		}
		if selectedProjectID != "" && projectID != selectedProjectID {
			continue
		}
		expiresAt, err := time.Parse(time.RFC3339, expiresAtText)
		if err != nil {
			return nil, err
		}
		details = append(details, fmt.Sprintf("%s: daily %s, issue %s, expires in %s, reason: %s", projectID, doctorOptionalBudgetUSD(dayCap), doctorOptionalBudgetUSD(issueCap), expiresAt.Sub(now).Round(time.Second), reason))
	}
	return details, rows.Err()
}

func doctorOptionalBudgetUSD(value sql.NullFloat64) string {
	if !value.Valid {
		return "base"
	}
	cap := value.Float64
	return optionalBudgetUSD(&cap)
}
