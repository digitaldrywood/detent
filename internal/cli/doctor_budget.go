package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/store"
)

func checkDoctorBudgetOverrides(ctx context.Context, resolution globalconfig.PathResolution, projectID string, now time.Time, deps doctorDeps) doctorCheck {
	check := doctorCheck{Name: "Budget overrides", Status: doctorOK, Detail: "no active temporary overrides"}
	path := strings.TrimSpace(resolution.Path)
	if path == "" {
		check.Status, check.Detail = doctorWarn, "skipped because the global config path is unavailable"
		return check
	}
	deps = deps.withDefaults()
	opts := defaultOptions()
	opts.lookupEnv, opts.httpDo = deps.lookupEnv, deps.httpDo
	owner, err := openHubBudgetOverrideStore(path, opts)
	if err != nil {
		check.Status, check.Detail = doctorWarn, fmt.Sprintf("could not read Hub overrides: %v", err)
		return check
	}
	var overrides []store.BudgetOverride
	if id := strings.TrimSpace(projectID); id != "" {
		value, readErr := owner.ActiveBudgetOverride(ctx, id, now)
		err = readErr
		if errors.Is(err, store.ErrNotFound) {
			err = nil
		} else if err == nil {
			overrides = append(overrides, value)
		}
	} else {
		overrides, err = owner.ListActiveBudgetOverrides(ctx, now)
	}
	if err != nil {
		check.Status, check.Detail = doctorWarn, fmt.Sprintf("could not list Hub overrides: %v", err)
		return check
	}
	details := make([]string, 0, len(overrides))
	for _, value := range overrides {
		details = append(details, fmt.Sprintf("%s: daily %s, issue %s, expires in %s, reason: %s", value.ProjectID, optionalBudgetUSD(value.PerDayMaxUSD), optionalBudgetUSD(value.PerIssueMaxUSD), value.ExpiresAt.Sub(now).Round(time.Second), value.Reason))
	}
	if len(details) > 0 {
		check.Status, check.Detail = doctorWarn, strings.Join(details, "; ")
		check.Hint = "Review temporary budget access and clear it early with detent budget override clear when it is no longer needed."
	}
	return check
}
