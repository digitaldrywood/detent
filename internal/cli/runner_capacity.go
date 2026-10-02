package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func runnerCapacityOwner(selected globalconfig.Config, runtimeConfig func() globalconfig.Config) func(context.Context, *runnerauth.CapacityRequest) *runnerauth.CapacityConfig {
	enrolled, enrolledErr := runnerauth.Load(selected.Client.IdentityFile)
	var mu sync.Mutex
	var lastApplication *runnerauth.CapacityConfig
	return func(ctx context.Context, request *runnerauth.CapacityRequest) *runnerauth.CapacityConfig {
		mu.Lock()
		defer mu.Unlock()
		if runtimeConfig == nil || enrolledErr != nil {
			return nil
		}
		report := observeRunnerCapacityConfiguration(selected, runtimeConfig(), enrolled)
		if report == nil {
			return nil
		}
		if request == nil {
			if lastApplication != nil && lastApplication.Revision == report.Revision && lastApplication.Constraint != "" && lastApplication.Constraint != "Waiting for the existing configuration reload to apply the saved limits." {
				report.Constraint = lastApplication.Constraint
				report.Manageable = lastApplication.Manageable
			}
			return report
		}
		defer func() {
			lastApplication = nil
			if report != nil {
				copy := *report
				lastApplication = &copy
			}
		}()
		if request.Validate() != nil {
			report.Constraint = "The selected configuration changed; read capacity and retry with its current revision."
			return report
		}
		if report.LocalLimit != request.Capacity || report.ClientLimit != request.Capacity {
			if ctx.Err() != nil {
				return report
			}
			applied := false
			err := globalconfig.Mutate(selected.Path, func(cfg *globalconfig.Config, revision string) bool {
				report = runnerCapacityConfiguration(selected, runtimeConfig(), *cfg, revision, enrolled)
				if report == nil || report.LocalLimit == request.Capacity && report.ClientLimit == request.Capacity {
					return false
				}
				if request.ExpectedConfigRevision != report.Revision {
					report.Constraint = "The selected configuration changed; read capacity and retry with its current revision."
					return false
				}
				if ctx.Err() != nil {
					return false
				}
				cfg.Global.MaxConcurrentAgents = request.Capacity
				cfg.Client.Capacity = request.Capacity
				applied = true
				return true
			}, globalconfig.WithProjectPathLiterals())
			if err != nil {
				report.Manageable = false
				report.Constraint = "The selected configuration could not be written; check the enrolled runner configuration permissions."
				return report
			}
			if applied {
				report = observeRunnerCapacityConfiguration(selected, runtimeConfig(), enrolled)
			}
			return report
		}
		return report
	}
}

func observeRunnerCapacityConfiguration(selected, runtime globalconfig.Config, enrolled runnerauth.File) *runnerauth.CapacityConfig {
	raw, err := os.ReadFile(selected.Path)
	if err != nil {
		return nil
	}
	cfg, err := globalconfig.Parse(raw, selected.Path, globalconfig.WithProjectPathLiterals())
	if err != nil {
		return nil
	}
	revision := sha256.Sum256(raw)
	return runnerCapacityConfiguration(selected, runtime, cfg, hex.EncodeToString(revision[:]), enrolled)
}

func runnerCapacityConfiguration(selected, runtime, cfg globalconfig.Config, revision string, enrolled runnerauth.File) *runnerauth.CapacityConfig {
	identity, err := runnerauth.Load(selected.Client.IdentityFile)
	if err != nil || identity.Identity.Binding != enrolled.Identity.Binding || identity.Identity.OrganizationID != enrolled.Identity.OrganizationID || identity.HubURL != enrolled.HubURL || cfg.Client.IdentityFile != selected.Client.IdentityFile || cfg.Client.ProviderCapacityFile != selected.Client.ProviderCapacityFile || cfg.Client.MachineID != "" && cfg.Client.MachineID != string(identity.Identity.MachineID) || cfg.Client.URL != selected.Client.URL || cfg.Client.OrganizationID != string(identity.Identity.OrganizationID) || runtime.Path != selected.Path {
		return nil
	}
	capacity := cfg.Client.Capacity
	if capacity <= 0 {
		capacity = cfg.Global.MaxConcurrentAgents
	}
	runtimeCapacity := runtime.Client.Capacity
	if runtimeCapacity <= 0 {
		runtimeCapacity = runtime.Global.MaxConcurrentAgents
	}
	report := &runnerauth.CapacityConfig{
		Revision: revision, LocalLimit: cfg.Global.MaxConcurrentAgents, ClientLimit: capacity,
		RuntimeLimit: min(runtime.Global.MaxConcurrentAgents, runtimeCapacity), Manageable: true, ObservedAt: time.Now().UTC(),
	}
	if report.RuntimeLimit != min(report.LocalLimit, report.ClientLimit) {
		report.Constraint = "Waiting for the existing configuration reload to apply the saved limits."
	}
	return report
}
