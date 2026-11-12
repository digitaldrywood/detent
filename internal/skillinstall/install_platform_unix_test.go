//go:build !windows

package skillinstall

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatorskill"
)

func TestInstallSecuresExactManagedFilePermissions(t *testing.T) {

	t.Parallel()

	config := testConfig(t, TargetCodex)
	if _, err := Install(config); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	destination := filepath.Join(config.Roots[TargetCodex].Skills, operatorskill.Directory)
	skillPath := filepath.Join(destination, operatorskill.SkillFile)
	if err := os.Chmod(skillPath, 0o644); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	result, err := Install(config)
	if err != nil {
		t.Fatalf("Install(reinstall) error = %v", err)
	}
	if result.Targets[0].Intent != "reinstall" || result.Targets[0].Status != "installed" {
		t.Fatalf("target = %#v, want permission repair", result.Targets[0])
	}
	assertFile(t, skillPath, operatorskill.Content(), fileMode)
	for _, action := range result.Actions {
		if action.Path == skillPath && action.Action != "secure_permissions" {
			t.Fatalf("skill action = %#v, want secure_permissions", action)
		}
	}
}
