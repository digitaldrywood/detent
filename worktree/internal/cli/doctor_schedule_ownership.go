package cli

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/coordination"
	"github.com/digitaldrywood/detent/internal/scheduleowner"
)

type doctorScheduleOwnershipProbe func(context.Context, workflowconfig.Config) (scheduleowner.Status, error)

func checkDoctorScheduleOwnership(
	ctx context.Context,
	projectID string,
	cfg workflowconfig.Config,
	deps doctorDeps,
) doctorCheck {
	name := "Project " + projectID + " schedule ownership"
	if !cfg.ScheduleOwnership.Enabled {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "scheduled operations are enabled but schedule_ownership.enabled is false",
			Hint:   "Commit a shared schedule_ownership backend and key before running schedulers on any host.",
		}
	}
	probe := deps.scheduleOwnership
	if probe == nil {
		probe = defaultDoctorScheduleOwnership
	}
	status, err := probe(ctx, cfg)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("no reachable schedule owner: %v", err),
			Hint:   "Fix the coordination repository, branch permissions, or GitHub credentials, then rerun detent doctor.",
		}
	}
	if !status.Active {
		detail := "coordination backend is reachable but no active schedule owner is present"
		if status.Owner != "" {
			detail = fmt.Sprintf("schedule owner %s generation %d expired at %s", status.Owner, status.Generation, doctorScheduleOwnershipTime(status.ExpiresAt))
		}
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: detail,
			Hint:   "Start one configured Detent daemon and confirm it can acquire and renew the project schedule lease.",
		}
	}
	return doctorCheck{
		Name:   name,
		Status: doctorOK,
		Detail: fmt.Sprintf("owner %s holds generation %d; renewed %s; expires no earlier than %s", status.Owner, status.Generation, doctorScheduleOwnershipTime(status.RenewedAt), doctorScheduleOwnershipTime(status.ExpiresAt)),
	}
}

func defaultDoctorScheduleOwnership(ctx context.Context, cfg workflowconfig.Config) (scheduleowner.Status, error) {
	coordinationEndpoint := ""
	if cfg.Tracker.Kind == workflowconfig.TrackerGitHub || cfg.Tracker.Kind == workflowconfig.TrackerGitHubLocal {
		coordinationEndpoint = cfg.Tracker.Endpoint
	}
	ownership := cfg.ScheduleOwnership.Normalized(cfg.Tracker.Repository, coordinationEndpoint)
	httpClient := ghconnector.NewPooledHTTPClient(ghconnector.HTTPTransportConfig{
		MaxIdleConns:        cfg.Tracker.HTTPMaxIdleConns,
		MaxIdleConnsPerHost: cfg.Tracker.HTTPMaxIdleConnsPerHost,
		IdleConnTimeout:     time.Duration(cfg.Tracker.HTTPIdleConnTimeoutMS) * time.Millisecond,
	})
	defer func() {
		if closeErr := httpClient.Close(); closeErr != nil {
			slog.Default().DebugContext(ctx, "close doctor schedule ownership client failed", "error", closeErr)
		}
	}()
	tokenSource := ghconnector.NewTokenResolver(ghconnector.TokenResolverConfig{
		Endpoint:                ownership.Endpoint,
		APIKey:                  cfg.Tracker.APIKey,
		GitHubAppID:             cfg.Tracker.GitHubAppID,
		GitHubAppPrivateKey:     cfg.Tracker.GitHubAppPrivateKey,
		GitHubAppPrivateKeyPath: cfg.Tracker.GitHubAppPrivateKeyPath,
		GitHubAppInstallationID: cfg.Tracker.GitHubAppInstallationID,
		HTTPClient:              httpClient,
	})
	client, err := ghconnector.NewClient(ghconnector.ClientConfig{
		Endpoint:    ownership.Endpoint,
		TokenSource: tokenSource,
		HTTPClient:  httpClient,
	})
	if err != nil {
		return scheduleowner.Status{}, err
	}
	store, err := coordination.NewGitHubRefStore(coordination.GitHubRefConfig{
		Repository: ownership.Repository,
		Branch:     ownership.Branch,
		Client:     client,
	})
	if err != nil {
		return scheduleowner.Status{}, err
	}
	manager, err := scheduleowner.New(ownership, "doctor", store, scheduleowner.Dependencies{})
	if err != nil {
		return scheduleowner.Status{}, err
	}
	return manager.Current(ctx)
}

func doctorScheduleOwnershipTime(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	return strings.TrimSpace(value.UTC().Format(time.RFC3339))
}
