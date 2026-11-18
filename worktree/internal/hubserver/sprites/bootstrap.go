package sprites

import (
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const spriteBootstrapSource = "https://raw.githubusercontent.com/digitaldrywood/detent/%s/scripts/sprite-runner-bootstrap.sh"

func ShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func BootstrapScript(organization tracker.OrganizationID, hubURL, setup, command, version string) string {
	if strings.TrimSpace(setup) == "" {
		setup = ":"
	}
	return fmt.Sprintf(`set -euo pipefail
set +x
export DETENT_ORGANIZATION_ID=%s
export DETENT_HUB_URL=%s
export TMPDIR="$HOME/detent-runner/.tmp"
mkdir -p -- "$TMPDIR"
printf "Customer setup started\n"
(
%s
) >/dev/null 2>&1
printf "Customer setup completed\nRunner bootstrap started\n"
bootstrap=$(mktemp "$TMPDIR/sprite-bootstrap.XXXXXX")
trap 'rm -f -- "$bootstrap"' EXIT
curl --fail --silent --show-error --location %s --output "$bootstrap" 2>&1
printf '%%s\n' %s | bash "$bootstrap" --version %s 2>&1
printf "Runner bootstrap completed\n"
`, ShellQuote(string(organization)), ShellQuote(hubURL), setup, ShellQuote(fmt.Sprintf(spriteBootstrapSource, version)), ShellQuote(command), ShellQuote(version))
}

func BootstrapDiagnostic(line string) string {
	switch line {
	case "Customer setup started", "Customer setup completed", "Runner bootstrap started", "Runner bootstrap completed":
		return line
	}
	for _, stage := range []struct{ prefix, safe string }{
		{"Installing Go ", "installing Go"},
		{"Installing GitHub CLI ", "installing GitHub CLI"},
		{"Installing ripgrep ", "installing ripgrep"},
		{"Installing fd ", "installing fd"},
		{"Installing PostgreSQL client ", "installing PostgreSQL client"},
		{"Installing Playwright Chromium and system dependencies", "installing Playwright Chromium and system dependencies"},
		{"Installing Docker ", "installing Docker"},
		{"Installing pinned Detent ", "installing Detent"},
	} {
		if strings.HasPrefix(line, stage.prefix) {
			return "Runner installer: " + stage.safe
		}
	}
	for _, completed := range []string{
		"none", "enrollment input and Sprite prerequisites checked", "distribution package candidates checked",
		"Go toolchain installed", "GitHub CLI, ripgrep and fd installed",
		"PostgreSQL client installed and executable verified", "agent CLIs and Playwright installed",
		"Chromium headless screenshot verified", "Docker installation checked (see capability result)",
		"Detent installed", "setup revision recorded",
		"runner registration command completed or stored identity reused; verify Hub connection",
		"runner Service configured; verify running state", "installation checkpoint created",
	} {
		if line == "Bootstrap stopped. Last completed step: "+completed+"." {
			return "Runner installer stopped after: " + completed
		}
	}
	for _, diagnostic := range []struct{ prefix, safe string }{
		{"E: Unable to locate package ", "APT package unavailable in configured repositories"},
		{"E: Version '", "APT requested package version unavailable"},
		{"E: Failed to fetch ", "APT repository download failed"},
		{"E: Could not get lock ", "APT package database locked"},
		{"E: The repository '", "APT repository rejected"},
		{"E: Unmet dependencies", "APT unmet dependencies"},
		{"The following packages have unmet dependencies:", "APT unmet dependencies"},
		{"dpkg: error processing package ", "package configuration failed"},
		{"sprite bootstrap: configured APT repositories provide no supported PostgreSQL client", "APT repositories provide no supported PostgreSQL client (18 or 17)"},
		{"sprite bootstrap: checksum mismatch for ", "download checksum mismatch"},
		{"sprite bootstrap: missing or invalid checksum for ", "download checksum missing or invalid"},
		{"curl: (6)", "download host could not be resolved"},
		{"curl: (7)", "download connection failed"},
		{"curl: (22)", "download HTTP request rejected"},
		{"curl: (28)", "download timed out"},
		{"curl: (60)", "download TLS certificate validation failed"},
	} {
		if strings.HasPrefix(line, diagnostic.prefix) {
			return "Runner installer: " + diagnostic.safe
		}
	}
	return ""
}
