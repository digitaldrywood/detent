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
		cfg, report := observeRunnerCapacityConfiguration(selected, runtimeConfig(), enrolled)
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
			if request.ExpectedConfigRevision != report.Revision {
				report.Constraint = "The selected configuration changed; read capacity and retry with its current revision."
				return report
			}
			if ctx.Err() != nil {
				return report
			}
			cfg.Global.MaxConcurrentAgents = request.Capacity
			cfg.Client.Capacity = request.Capacity
			if err := globalconfig.Write(selected.Path, cfg, globalconfig.WithProjectPathLiterals()); err != nil {
				report.Manageable = false
				report.Constraint = "The selected configuration could not be written; check the enrolled runner configuration permissions."
				return report
			}
			_, report = observeRunnerCapacityConfiguration(selected, runtimeConfig(), enrolled)
			return report
		}
		return report
	}
}

func observeRunnerCapacityConfiguration(selected, runtime globalconfig.Config, enrolled runnerauth.File) (globalconfig.Config, *runnerauth.CapacityConfig) {
	raw, err := os.ReadFile(selected.Path)
	if err != nil {
		return globalconfig.Config{}, nil
	}
	cfg, err := globalconfig.Parse(raw, selected.Path, globalconfig.WithProjectPathLiterals())
	if err != nil {
		return globalconfig.Config{}, nil
	}
	identity, err := runnerauth.Load(selected.Client.IdentityFile)
	if err != nil || identity.Identity.Binding != enrolled.Identity.Binding || identity.Identity.OrganizationID != enrolled.Identity.OrganizationID || identity.HubURL != enrolled.HubURL || cfg.Client.IdentityFile != selected.Client.IdentityFile || cfg.Client.ProviderCapacityFile != selected.Client.ProviderCapacityFile || cfg.Client.MachineID != "" && cfg.Client.MachineID != string(identity.Identity.MachineID) || cfg.Client.URL != selected.Client.URL || cfg.Client.OrganizationID != string(identity.Identity.OrganizationID) || runtime.Path != selected.Path {
		return globalconfig.Config{}, nil
	}
	revision := sha256.Sum256(raw)
	capacity := cfg.Client.Capacity
	if capacity <= 0 {
		capacity = cfg.Global.MaxConcurrentAgents
	}
	runtimeCapacity := runtime.Client.Capacity
	if runtimeCapacity <= 0 {
		runtimeCapacity = runtime.Global.MaxConcurrentAgents
	}
	report := &runnerauth.CapacityConfig{
		Revision: hex.EncodeToString(revision[:]), LocalLimit: cfg.Global.MaxConcurrentAgents, ClientLimit: capacity,
		RuntimeLimit: min(runtime.Global.MaxConcurrentAgents, runtimeCapacity), Manageable: true, ObservedAt: time.Now().UTC(),
	}
	if report.RuntimeLimit != min(report.LocalLimit, report.ClientLimit) {
		report.Constraint = "Waiting for the existing configuration reload to apply the saved limits."
	}
	return cfg, report
}
