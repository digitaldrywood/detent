package runner

import (
	"context"
	"maps"
	"strings"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func (r *Runner) withGoBudget(ctx context.Context, workspacePath string, environment procgroup.Environment) procgroup.Environment {
	if root := toolcache.ManagedBuildFromContext(ctx); root != "" {
		environment.Variables = maps.Clone(environment.Variables)
		if environment.Variables == nil {
			environment.Variables = make(map[string]string)
		}
		environment.Variables["GOCACHE"] = root
	}
	if strings.TrimSpace(workspacePath) == "" {
		return environment
	}
	inherited, explicit := environment.Variables["GOFLAGS"]
	if !explicit && r.lookupEnv != nil {
		inherited = r.lookupEnv("GOFLAGS")
	}
	variables, err := r.goBudget.Environment(inherited)
	if err != nil {
		r.logger.Warn("go build budget unavailable; worker Go commands run unqueued", "error", err)
		return environment
	}
	if len(variables) == 0 {
		return environment
	}
	merged := make(map[string]string, len(environment.Variables)+len(variables))
	for key, value := range variables {
		merged[key] = value
	}
	if r.lookupEnv != nil {
		if value := r.lookupEnv("GOMAXPROCS"); value != "" {
			merged["GOMAXPROCS"] = value
		}
	}
	for key, value := range environment.Variables {
		if key == "GOFLAGS" {
			continue
		}
		merged[key] = value
	}
	environment.Variables = merged
	return environment
}
