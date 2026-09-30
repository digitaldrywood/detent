package runner

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

func isGoModule(workspacePath string) bool {
	if strings.TrimSpace(workspacePath) == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(workspacePath, "go.mod"))
	return err == nil
}

func (r *Runner) withGoBudget(workspacePath string, environment procgroup.Environment) procgroup.Environment {
	if !isGoModule(workspacePath) {
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
	for key, value := range environment.Variables {
		if key == "GOFLAGS" {
			continue
		}
		merged[key] = value
	}
	environment.Variables = merged
	return environment
}
