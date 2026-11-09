package hubserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSpriteProvisionLostResponseReconcilesAfterReopen(t *testing.T) {
	for _, test := range []struct{ name, mode string }{
		{"matching creation identity", "matching"},
		{"foreign creation identity", "foreign"},
		{"missing creation identity", "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var poolConfig *pgxpool.Config
			repository, _ := projectRecordPostgresWithAdmin(t, 3, func(cfg *pgxpool.Config) { poolConfig = cfg.Copy() })
			now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
			details := &verifiedAttemptDetails{}
			keys := secretTestKeys(t, "1", "1")
			type resourceRecord struct {
				ID     string
				Labels []string
			}
			resources := map[string]resourceRecord{}
			var resourceKeys []string
			creates := 0
			client := &http.Client{Transport: spritesTestTransport(func(request *http.Request) (*http.Response, error) {
				name := strings.TrimPrefix(request.URL.Path, "/v1/sprites/")
				if request.Method == http.MethodPost {
					var body struct {
						Name   string   `json:"name"`
						Labels []string `json:"labels"`
					}
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
						return nil, err
					}
					name = body.Name
					key := request.Header.Get("Authorization") + name
					id := "resource-" + name
					if test.mode == "foreign" {
						id = "foreign-" + name
					}
					resources[key] = resourceRecord{ID: id, Labels: append([]string(nil), body.Labels...)}
					resourceKeys = append(resourceKeys, key)
					creates++
					return nil, errors.New("provider response was lost")
				}
				key := request.Header.Get("Authorization") + name
				resource, exists := resources[key]
				status := http.StatusNotFound
				raw := "{}"
				if exists {
					status = http.StatusOK
					labels := resource.Labels
					if test.mode == "missing" {
						labels = nil
					}
					if test.mode == "foreign" {
						for i, candidate := range resourceKeys {
							if candidate == key {
								labels = resources[resourceKeys[i^1]].Labels
								break
							}
						}
					}
					encoded, err := json.Marshal(map[string]any{"id": resource.ID, "name": name, "labels": labels})
					if err != nil {
						return nil, err
					}
					raw = string(encoded)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(raw)), Header: make(http.Header)}, nil
			})}
			services := map[tracker.OrganizationID]*Service{}
			for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
				actor := "admin_" + string(organization)
				envelope, err := keys.Seal([]byte(string(organization)), accessstore.SecretAAD(organization, "", flySpritesToken))
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", Slug: string(organization), CreatedAt: now}); err != nil {
						return err
					}
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: actor, Hash: actor, Name: "Administrator", Kind: "service", Scope: "admin", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
						return err
					}
					if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: flySpritesToken, OrganizationSlug: "example", Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Actor: actor, Kind: flySpritesToken, Event: "set", KeyVersion: envelope.Version, RecordedAt: now}); err != nil {
						return err
					}
					owner, ok := w.(postgres.SpriteWriter)
					if !ok {
						return accessstore.ErrTransactionRequired
					}
					if _, err := owner.PutSpritePool(t.Context(), postgres.SpritePool{OrganizationID: organization, MaxRunners: 2, IdleSeconds: 300, ConfiguredBy: actor, IsolationTier: "native-trusted", PlacementMode: "blended"}, 0); err != nil {
						return err
					}
					for _, project := range []tracker.ProjectID{"first", "second"} {
						if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
							return err
						}
						if _, err := owner.PutSpriteAllocation(t.Context(), postgres.SpriteAllocation{OrganizationID: organization, ProjectID: project, DesiredCount: 1, ConfiguredBy: actor, RequestedAt: now}, 0); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				service := &Service{database: &database{postgres: repository, billingRepository: repository}, config: Config{AccessRepository: repository, TenantRepository: repository, Hosted: &HostedConfig{OrganizationID: string(organization), PublicURL: "https://hub.example.test"}, SecretKeys: keys, RunDetails: details, SpritesHTTPClient: client, Version: "v0.1.0", now: func() time.Time { return now }, generateToken: apikey.GenerateToken}}
				services[organization] = service
				for _, name := range []string{"first", "second"} {
					reserved, err := service.createPoolSpriteRecord(t.Context(), nativeScope{organization: organization}, spritePoolSettings{Revision: 1}, false, name)
					if !reserved || err == nil {
						t.Fatalf("lost response reservation=%t error=%v", reserved, err)
					}
				}
			}
			if creates != 4 {
				t.Fatalf("provider creates=%d want 4", creates)
			}
			if err := repository.Close(); err != nil {
				t.Fatal(err)
			}
			pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			reopened, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := reopened.Close(); err != nil {
					t.Error(err)
				}
			})
			now = now.Add(time.Minute)
			firstOperationKey := ""
			for organization, service := range services {
				service.config.TenantRepository, service.config.AccessRepository = reopened, reopened
				service.database.billingRepository = reopened
				var pending []postgres.SpriteOperation
				if err := reopened.ReadSprite(t.Context(), organization, corestore.Serving, func(r postgres.SpriteReader) error {
					var err error
					pending, err = r.PendingSpriteOperations(t.Context(), now, 20)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if len(pending) != 2 {
					t.Fatalf("durable uncertain operations=%d want 2", len(pending))
				}
				seen := map[tracker.ProjectID]bool{}
				for _, op := range pending {
					if organization == "org_first" {
						firstOperationKey = op.IdempotencyKey
					}
					if op.Outcome != "uncertain" || op.RequestObjectKey == nil || op.TargetEnrollmentID == "" {
						t.Fatalf("incomplete uncertain reservation: %+v", op)
					}
					seen[op.ProjectID] = true
					err := service.runSpriteOperation(t.Context(), op)
					if test.mode == "matching" && err != nil {
						t.Fatal(err)
					}
					if test.mode != "matching" && err == nil {
						t.Fatal("uncertain creation adopted an unowned provider resource")
					}
					if err := reopened.ReadSprite(t.Context(), organization, corestore.Serving, func(r postgres.SpriteReader) error {
						current, err := r.SpriteOperation(t.Context(), op.IdempotencyKey)
						if err != nil {
							return err
						}
						if test.mode != "matching" {
							if current.Outcome != "uncertain" || current.ProviderResourceID != "" || current.ReceiptObjectKey != nil || !current.NextAttemptAt.After(now) {
								t.Fatalf("foreign resource became a receipt: %+v", current)
							}
							if _, err := r.SpriteOperation(t.Context(), "bootstrap:"+op.TargetEnrollmentID); !errors.Is(err, corestore.ErrNotFound) {
								t.Fatalf("unowned resource received bootstrap intent: %v", err)
							}
							return nil
						}
						if current.Outcome != "committed" || current.ProviderResourceID != "resource-"+op.Name || current.ReceiptObjectKey == nil {
							t.Fatalf("unreconciled operation=%+v", current)
						}
						bootstrap, err := r.SpriteOperation(t.Context(), "bootstrap:"+op.TargetEnrollmentID)
						if err != nil {
							return err
						}
						if bootstrap.TargetEnrollmentID != op.TargetEnrollmentID || bootstrap.ProjectID != op.ProjectID {
							t.Fatal("next phase lost its tenant reservation")
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if !seen["first"] || !seen["second"] {
					t.Fatalf("allocation fairness lost a project: %v", seen)
				}
			}
			if creates != 4 {
				t.Fatalf("reconciliation repeated provider creation: %d", creates)
			}
			if firstOperationKey == "" {
				t.Fatal("missing source tenant operation")
			}
			if err := reopened.ReadSprite(t.Context(), "org_second", corestore.Serving, func(r postgres.SpriteReader) error {
				_, err := r.SpriteOperation(t.Context(), firstOperationKey)
				return err
			}); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("foreign tenant operation lookup=%v", err)
			}
		})
	}
}
func TestSpriteOperationRetrySpreadsEqualLengthIdentities(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		attempts int64
		delay    time.Duration
	}{
		{"initial retry", 0, time.Second},
		{"growing retry", 1, 2 * time.Second},
		{"capped retry", 8, 256 * time.Second},
		{"later retry stays capped", 20, 256 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			seen := map[time.Time]bool{}
			for i := range 32 {
				op := postgres.SpriteOperation{OrganizationID: "org_first", IdempotencyKey: "provision:enrollment-" + strconv.Itoa(1000+i), Attempts: test.attempts}
				at := spriteOperationRetryAt(op, now)
				if at.Before(now.Add(test.delay)) || !at.Before(now.Add(test.delay+31*time.Millisecond)) {
					t.Fatalf("retry delay=%s outside bounded backoff", at.Sub(now))
				}
				if again := spriteOperationRetryAt(op, now); !again.Equal(at) {
					t.Fatal("persisted identity changed its retry schedule")
				}
				seen[at] = true
			}
			if len(seen) < 10 {
				t.Fatalf("equal-length operation identities produced only %d retry times", len(seen))
			}
			op := postgres.SpriteOperation{OrganizationID: "org_first", IdempotencyKey: "provision:enrollment-1000", Attempts: test.attempts}
			first := spriteOperationRetryAt(op, now)
			op.OrganizationID = "org_second"
			if spriteOperationRetryAt(op, now).Equal(first) {
				t.Fatal("tenant identity did not separate retry schedules")
			}
		})
	}
}

func TestSpritePoolBootstrapShell(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, setup     string
		installer, want int
	}{
		{"blank", "", 0, 0},
		{"whitespace", " \n\t", 0, 0},
		{"carriage return", " \r\n\t", 0, 0},
		{"no-op", ":", 0, 0},
		{"customer command", "printf 'customer-secret\\n'; touch \"$HOME/customer-ran\"", 0, 0},
		{"customer failure", "printf 'customer-secret\\n'; exit 23", 0, 23},
		{"installer failure", ":", 100, 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			installer := filepath.Join(root, "installer")
			if err := os.WriteFile(installer, []byte("cat >/dev/null\nprintf 'Installing PostgreSQL client 18.6\\n'\nif [[ "+strconv.Itoa(test.installer)+" != 0 ]]; then printf \"E: Version 'installer-secret' for 'postgresql-client-17' was not found\\n\" >&2; fi\nprintf 'Authorization: Bearer provider-secret\\n' >&2\nexit "+strconv.Itoa(test.installer)+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			curl := "#!/usr/bin/env bash\nwhile [[ $1 != --output ]]; do shift; done\ncp -- \"$FIXTURE_INSTALLER\" \"$2\"\n"
			if err := os.WriteFile(filepath.Join(root, "curl"), []byte(curl), 0700); err != nil {
				t.Fatal(err)
			}
			script := spritePoolBootstrapScript("org_example", "https://example.com", test.setup, "enrollment-secret", "v0.1.0")
			cmd := exec.CommandContext(t.Context(), "bash", "-s")
			cmd.Env = append(os.Environ(), "HOME="+root, "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "FIXTURE_INSTALLER="+installer)
			cmd.Stdin = strings.NewReader(script)
			output, err := cmd.CombinedOutput()
			status := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				status = exit.ExitCode()
			}
			if status != test.want {
				t.Fatalf("exit=%d, want=%d; output=%s", status, test.want, output)
			}
			if strings.Contains(string(output), "customer-secret") || strings.Contains(string(output), "enrollment-secret") {
				t.Fatal("bootstrap exposed sensitive input")
			}
			if strings.Contains(string(output), "Runner bootstrap completed") != (test.want == 0) {
				t.Fatalf("incorrect completion: %s", output)
			}
			_, markerErr := os.Stat(filepath.Join(root, "customer-ran"))
			if (markerErr == nil) != (test.name == "customer command") {
				t.Fatalf("customer command did not retain its behavior: %v", markerErr)
			}
			var log strings.Builder
			for _, line := range strings.Split(string(output), "\n") {
				if safe := spriteBootstrapDiagnostic(line); safe != "" {
					log.WriteString(safe + "\n")
				}
			}
			for _, secret := range []string{"customer-secret", "enrollment-secret", "installer-secret", "provider-secret"} {
				if strings.Contains(log.String(), secret) {
					t.Fatal("sensitive marker reached returned bootstrap logs")
				}
			}
			if test.installer != 0 && !strings.Contains(log.String(), "Runner installer: APT requested package version unavailable") {
				t.Fatalf("installer stderr diagnostic discarded: %s", log.String())
			}
			t.Logf("exit=%d; retained stdout/stderr after allowlisting:\n%s", status, log.String())
		})
	}
}
