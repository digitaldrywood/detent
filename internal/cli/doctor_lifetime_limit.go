package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/store"
)

func checkDoctorLifetimeLimits(ctx context.Context, projectID, storePath string, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	name := "Project " + projectID + " lifetime limits"
	if strings.TrimSpace(storePath) == "" {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "no completed issue history recorded yet"}
	}
	deps = deps.withDefaults()
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		if errors.Is(err, errDoctorTelemetryStoreUnavailable) {
			return doctorCheck{Name: name, Status: doctorOK, Detail: "no completed issue history recorded yet"}
		}
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("completed issue history could not be read: %v", err)}
	}
	history, queryErr := store.QueryProjectLifetimeUsage(ctx, db, projectID)
	closeErr := db.Close()
	if queryErr != nil {
		if strings.Contains(strings.ToLower(queryErr.Error()), "no such table") {
			return doctorCheck{Name: name, Status: doctorOK, Detail: "no completed issue history recorded yet"}
		}
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("completed issue history query failed: %v", queryErr)}
	}
	if closeErr != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("completed issue history close failed: %v", closeErr)}
	}
	if history.CompletedIssues == 0 {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "no completed issue history recorded yet"}
	}

	sessionLimit := cfg.Agent.LifetimeSessionLimit
	tokenLimit := cfg.Agent.LifetimeTokenLimit
	contextDetail := fmt.Sprintf(
		"project p95 from %d completed issues: sessions %d, tokens %d",
		history.CompletedIssues,
		history.P95Sessions,
		history.P95Tokens,
	)
	below := make([]string, 0, 2)
	if sessionLimit > 0 && sessionLimit < history.P95Sessions {
		below = append(below, fmt.Sprintf("agent.lifetime_session_limit %d is below project p95 %d", sessionLimit, history.P95Sessions))
	}
	if tokenLimit > 0 && tokenLimit < history.P95Tokens {
		below = append(below, fmt.Sprintf("agent.lifetime_token_limit %d is below project p95 %d", tokenLimit, history.P95Tokens))
	}
	if len(below) > 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: strings.Join(below, "; ") + "; " + contextDetail,
			Hint:   "Raise each configured lifetime limit to at least its project p95, or set it to 0 to disable that limit deliberately.",
		}
	}
	if sessionLimit == 0 && tokenLimit == 0 {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "lifetime limits are disabled; " + contextDetail}
	}
	return doctorCheck{Name: name, Status: doctorOK, Detail: "configured lifetime limits are not below " + contextDetail}
}
