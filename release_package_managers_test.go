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
		"token: \"{{ .Env.SCOOP_BUCKET_GITHUB_TOKEN }}\"",
		"skip_upload: \"{{ if index .Env \\\"SCOOP_BUCKET_GITHUB_TOKEN\\\" }}false{{ else }}true{{ end }}\"",
		"winget:",
		"package_identifier: DigitalDrywood.Detent",
		"name: winget-pkgs",
		"branch: detent-{{ .Version }}",
		"token: \"{{ .Env.WINGET_GITHUB_TOKEN }}\"",
		"owner: microsoft",
		"branch: master",
		"skip_upload: \"{{ if index .Env \\\"WINGET_GITHUB_TOKEN\\\" }}false{{ else }}true{{ end }}\"",
		"installation_notes: Installs detent.exe on PATH. Verify the release with detent --version.",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf(".goreleaser.yaml missing %q", want)
		}
	}
}

func TestWindowsPackageManagerDocs(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("ReadFile(README.md) error = %v", err)
	}
	readme := string(raw)

	for _, want := range []string{
		"winget install --id DigitalDrywood.Detent --source winget",
		"scoop bucket add digitaldrywood https://github.com/digitaldrywood/scoop-bucket",
		"scoop install detent",
		"irm https://raw.githubusercontent.com/digitaldrywood/detent/main/install.ps1 | iex",
		"go install github.com/digitaldrywood/detent/cmd/detent@latest",
		"detent.exe` on PATH; verify any Windows install with `detent --version`",
		"winget upgrade --id DigitalDrywood.Detent",
		"scoop update detent",
		"brew upgrade digitaldrywood/tap/detent",
		"SCOOP_BUCKET_GITHUB_TOKEN",
		"WINGET_GITHUB_TOKEN",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("README.md missing %q", want)
		}
	}
}
