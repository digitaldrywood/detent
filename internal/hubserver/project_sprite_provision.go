package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const spriteBootstrapSource = "https://raw.githubusercontent.com/digitaldrywood/detent/%s/scripts/sprite-runner-bootstrap.sh"

func spriteShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (s *Service) poolSpriteToken(ctx context.Context, scope nativeScope, name string) ([]byte, error) {
	var operation postgres.SpriteOperation
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		member, err := owner.SpriteMember(ctx, name)
		if err != nil {
			return err
		}
		operation, err = owner.SpriteOperation(ctx, "provision:"+member.EnrollmentID)
		return err
	})
	if err != nil {
		return nil, err
	}
	request, err := s.spriteLifecyclePayload(ctx, operation)
	if err != nil {
		return nil, err
	}
	return s.spriteOperationToken(ctx, operation, request)
}

func (s *Service) spritePoolClient() *http.Client {
	client := http.Client{}
	if s.config.SpritesHTTPClient != nil {
		client = *s.config.SpritesHTTPClient
	}
	client.Timeout = 0
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func (s *Service) createPoolSprite(ctx context.Context, scope nativeScope, settings spritePoolSettings, allowOrganization bool) (bool, error) {
	return s.createPoolSpriteWithNames(ctx, scope, settings, allowOrganization, &spriteNameGenerator{intN: rand.IntN})
}

func (s *Service) createPoolSpriteWithNames(ctx context.Context, scope nativeScope, settings spritePoolSettings, allowOrganization bool, names *spriteNameGenerator) (bool, error) {
	if s.config.Hosted == nil || settings.MaxRunners == 0 {
		return false, nil
	}
	created := false
	for {
		name, err := names.next(func(name string) (bool, error) {
			exists := false
			err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				owner, ok := r.(postgres.SpriteReader)
				if !ok {
					return accessstore.ErrTransactionRequired
				}
				_, err := owner.SpriteMember(ctx, name)
				exists = err == nil
				if errors.Is(err, corestore.ErrNotFound) {
					return nil
				}
				return err
			})
			return exists, err
		})
		if err != nil {
			return created, err
		}
		reserved, err := s.createPoolSpriteRecord(ctx, scope, settings, allowOrganization, name)
		created = created || reserved
		if !errors.Is(err, errSpriteNameTaken) {
			return created, err
		}
	}
}

func (s *Service) createPoolSpriteNamed(ctx context.Context, scope nativeScope, settings spritePoolSettings, allowOrganization bool, name string) (bool, error) {
	return s.createPoolSpriteRecord(ctx, scope, settings, allowOrganization, name)
}

func spritePoolBootstrapScript(organization tracker.OrganizationID, hubURL, setup, command, version string) string {
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
`, spriteShellQuote(string(organization)), spriteShellQuote(hubURL), setup, spriteShellQuote(fmt.Sprintf(spriteBootstrapSource, version)), spriteShellQuote(command), spriteShellQuote(version))
}

func spriteBootstrapDiagnostic(line string) string {
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

func (s *Service) deletePoolSprite(ctx context.Context, scope nativeScope, member spritePoolMember) error {
	return s.deletePoolSpriteRecord(ctx, scope, member)
}

func spriteNameCollision(status int, data []byte) bool {
	if status == http.StatusConflict {
		return true
	}
	if status != http.StatusBadRequest {
		return false
	}
	var failure struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &failure) != nil {
		return false
	}
	message := strings.ToLower(failure.Error)
	return strings.Contains(message, "already exists") || strings.Contains(message, "already taken")
}
