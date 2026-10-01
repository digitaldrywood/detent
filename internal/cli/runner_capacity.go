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
	return func(ctx context.Context, request *runnerauth.CapacityRequest) *runnerauth.CapacityConfig {
		mu.Lock()
		defer mu.Unlock()
		if runtimeConfig == nil || enrolledErr != nil {
			return nil
		}
		runtime := runtimeConfig()
		raw, err := os.ReadFile(selected.Path)
		if err != nil {
			return nil
		}
		cfg, err := globalconfig.Parse(raw, selected.Path, globalconfig.WithProjectPathLiterals())
		if err != nil {
			return nil
		}
		identity, err := runnerauth.Load(selected.Client.IdentityFile)
		if err != nil || identity.Identity.Binding != enrolled.Identity.Binding || identity.Identity.OrganizationID != enrolled.Identity.OrganizationID || identity.HubURL != enrolled.HubURL || cfg.Client.IdentityFile != selected.Client.IdentityFile || cfg.Client.ProviderCapacityFile != selected.Client.ProviderCapacityFile || cfg.Client.MachineID != "" && cfg.Client.MachineID != string(identity.Identity.MachineID) || cfg.Client.URL != selected.Client.URL || cfg.Client.OrganizationID != string(identity.Identity.OrganizationID) || runtime.Path != selected.Path {
			return nil
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
		if request != nil {
			if request.Validate() != nil {
				report.Constraint = "The selected configuration changed; read capacity and retry with its current revision."
				return report
			}
			if cfg.Global.MaxConcurrentAgents != request.Capacity || capacity != request.Capacity {
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
				report.Constraint = "Waiting for the existing configuration reload to apply the saved limits."
				return report
			}
		}
		if report.RuntimeLimit != min(report.LocalLimit, report.ClientLimit) {
			report.Constraint = "Waiting for the existing configuration reload to apply the saved limits."
		}
		return report
	}
}
