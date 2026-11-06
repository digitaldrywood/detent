package cli

import (
	"context"
	"strings"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/providercapacity"
)

func checkDoctorProviderCapacity(ctx context.Context, cfg globalconfig.Config) doctorCheck {
	check := doctorCheck{Name: "Runner provider capacity", Status: doctorOK}
	if _, err := providercapacity.Load(cfg.Client.ProviderCapacityFile); err != nil {
		check.Status, check.Detail = doctorFail, err.Error()
		check.Hint = "Publish a valid bounded provider report file; keep credentials in the local backend configuration."
		return check
	}
	fleet, err := newHubRunnerFleet(cfg)
	if err != nil {
		check.Status, check.Detail = doctorFail, err.Error()
		return check
	}
	view, err := fleet.Fleet(ctx)
	if err != nil {
		check.Status, check.Detail = doctorWarn, err.Error()
		return check
	}
	var details []string
	for _, runner := range view.Runners {
		for _, capacity := range runner.ProviderCapacity {
			check.ProviderCapacity = append(check.ProviderCapacity, capacity)
			details = append(details, capacity.Summary())
			if capacity.State != "available" || capacity.Used >= capacity.MaxConcurrent {
				check.Status = doctorWarn
			}
		}
	}
	check.Detail = strings.Join(details, "; ")
	if len(details) == 0 {
		check.Status, check.Detail = doctorWarn, "Hub has not received a provider report from this runner"
	}
	return check
}
