package detent_test

import (
	"os"
	"strings"
	"testing"
)

func TestGoReleaserWindowsPackageManagerConfig(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(".goreleaser.yaml")
	if err != nil {
		t.Fatalf("ReadFile(.goreleaser.yaml) error = %v", err)
	}
	config := string(raw)

	for _, want := range []string{
		"scoops:",
		"name: scoop-bucket",
		"token: \"{{ index .Env \\\"SCOOP_BUCKET_GITHUB_TOKEN\\\" }}\"",
		"skip_upload: \"{{ if index .Env \\\"SCOOP_BUCKET_GITHUB_TOKEN\\\" }}auto{{ else }}true{{ end }}\"",
		"winget:",
		"package_identifier: DigitalDrywood.Detent",
		"name: winget-pkgs",
		"branch: detent-{{ .Version }}",
		"token: \"{{ index .Env \\\"WINGET_GITHUB_TOKEN\\\" }}\"",
		"owner: microsoft",
		"branch: master",
		"skip_upload: \"{{ if index .Env \\\"WINGET_GITHUB_TOKEN\\\" }}auto{{ else }}true{{ end }}\"",
		"installation_notes: Installs detent.exe on PATH. Verify the release with detent --version.",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf(".goreleaser.yaml missing %q", want)
		}
	}
}
