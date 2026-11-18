//go:build !windows

package detent_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCollectReleaseRulesetsFailsClosed(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Fatalf("bash is unavailable: %v", err)
	}

	tests := []struct {
		name       string
		scenario   string
		wantErr    bool
		wantOutput string
	}{
		{name: "list fails after partial output", scenario: "list-failure", wantErr: true, wantOutput: "existing\n"},
		{name: "detail fails after partial output", scenario: "detail-failure", wantErr: true, wantOutput: "existing\n"},
		{name: "complete discovery replaces output", scenario: "success", wantOutput: "{\"id\":101}\n{\"id\":202}\n"},
		{name: "successful empty discovery replaces output", scenario: "empty", wantOutput: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			fakeBin := filepath.Join(dir, "bin")
			if err := os.Mkdir(fakeBin, 0o755); err != nil {
				t.Fatalf("Mkdir(fake bin) error = %v", err)
			}
			fakeGH := filepath.Join(fakeBin, "gh")
			if err := os.WriteFile(fakeGH, []byte(fakeRulesetGHScript), 0o755); err != nil {
				t.Fatalf("WriteFile(fake gh) error = %v", err)
			}

			output := filepath.Join(dir, "rulesets.json")
			if err := os.WriteFile(output, []byte("existing\n"), 0o600); err != nil {
				t.Fatalf("WriteFile(existing output) error = %v", err)
			}

			cmd := exec.CommandContext(t.Context(), "bash", "scripts/collect-release-rulesets.sh", "digitaldrywood/detent", output)
			cmd.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"), "TEST_SCENARIO="+tt.scenario)
			combined, err := cmd.CombinedOutput()
			if tt.wantErr && err == nil {
				t.Fatalf("collect rulesets unexpectedly succeeded; output:\n%s", combined)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("collect rulesets error = %v; output:\n%s", err, combined)
			}

			got, err := os.ReadFile(output)
			if err != nil {
				t.Fatalf("ReadFile(output) error = %v", err)
			}
			if string(got) != tt.wantOutput {
				t.Fatalf("output = %q, want %q", got, tt.wantOutput)
			}
		})
	}
}
