package hubserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/budget"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newSpriteNativeFixture(t *testing.T, cfg Config) nativeFixture {
	t.Helper()
	if cfg.SecretKeys == nil {
		cfg.SecretKeys = secretTestKeys(t, "1", "1")
	}
	service := openTestService(t, cfg)
	service.workerCancel()
	<-service.workerDone
	<-service.secretRewrapDone
	organization := tracker.OrganizationID(service.config.Hosted.OrganizationID)
	service.config.Hosted = nil
	service.localOrganization = string(organization)
	service.database.hostedOrganization = organization
	service.requestMetrics.organization = organization
	service.echo = echo.New()
	service.echo.Use(service.measureRequests, service.policyCacheMiddleware, service.changeContentMiddleware, service.runDetailMiddleware)
	service.registerRoutes(service.echo)
	key := bytes.Repeat([]byte{0x5a}, 32)
	envelope, err := service.config.SecretKeys.Seal(key, accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	now := service.config.now().UTC().Truncate(time.Microsecond)
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: newNativeID("hub"), CursorKey: key}); err != nil {
			return err
		}
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	return newNativeFixture(t, service, organization, "Example project")
}

func assertSpritePostgresFacts(t *testing.T, f nativeFixture) {
	t.Helper()
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(runnerstore.FleetReader)
		if !ok {
			return runnerstore.ErrInvalidRecord
		}
		records, err := reader.FleetRecords(t.Context(), 200)
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.Machine.SpriteName != "" && record.Machine.SpriteName != record.Machine.Hostname {
				t.Fatalf("Sprite machine identity disagrees with its hostname")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSpritePoolPostgresSettingsAndAuthority(t *testing.T) {
	var poolConfig *pgxpool.Config
	repository, _ := projectRecordPostgresWithAdmin(t, 3, func(cfg *pgxpool.Config) { poolConfig = cfg.Copy() })
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	details := &verifiedAttemptDetails{}
	services := make(map[tracker.OrganizationID]*Service)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		principal := "admin_" + string(org)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", Slug: string(org), CreatedAt: now}); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"first", "second"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
			}
			return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: principal, Hash: principal, Name: "Example administrator", Scope: "admin", Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		service := &Service{database: &database{postgres: repository}, config: Config{AccessRepository: repository, TenantRepository: repository, Hosted: &HostedConfig{OrganizationID: string(org)}, SecretKeys: secretTestKeys(t, "1", "1"), RunDetails: details, now: func() time.Time { return now }}}
		scope := nativeScope{organization: org, project: "first", credential: apiCredential{ID: principal, Hash: principal, Scope: apiScopeAdmin}}
		services[org] = service
		settings := spritePoolSettings{MinRunners: 1, MaxRunners: 3, IdleSeconds: 120, Bootstrap: "printf " + string(org), Placement: &policy.Placement{Mode: "local_first", TodoThreshold: 3, OverflowSlots: 2}}
		if err := service.updateSpritePool(t.Context(), scope, settings, nil); err != nil {
			t.Fatal(err)
		}
		got, row, err := service.spritePoolSettingsRecord(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		if got.Revision != 1 || got.Bootstrap != settings.Bootstrap || got.MinRunners != 1 || got.MaxRunners != 3 || got.IdleSeconds != 120 || *got.Placement != *settings.Placement || row.ConfiguredBy != principal || row.BootstrapObjectKey == nil {
			t.Fatalf("persisted settings=%+v owner=%+v", got, row)
		}
		command := spritePoolCommand{Mutation: tracker.Mutation{IdempotencyKey: "settings-command"}, spritePoolSettings: spritePoolSettings{Revision: 1, MinRunners: 1, MaxRunners: 4, IdleSeconds: 180}, ShareIdleCapacity: new(true)}
		if err := service.executeSpritePoolCommand(t.Context(), scope, command); err != nil {
			t.Fatal(err)
		}
		if err := service.executeSpritePoolCommand(t.Context(), scope, command); err != nil {
			t.Fatalf("replay=%v", err)
		}
		for _, test := range []struct {
			name    string
			scope   nativeScope
			command spritePoolCommand
		}{
			{"changed command", scope, spritePoolCommand{Mutation: command.Mutation, spritePoolSettings: spritePoolSettings{Revision: 1, MaxRunners: 5}}},
			{"stale settings", scope, spritePoolCommand{spritePoolSettings: spritePoolSettings{Revision: 1, MaxRunners: 3}}},
			{"sandbox", scope, spritePoolCommand{spritePoolSettings: spritePoolSettings{Revision: 2, MaxRunners: 3}, IsolationTier: new("sandbox")}},
			{"foreign organization", nativeScope{organization: "org_foreign", credential: scope.credential}, command},
			{"missing credential", nativeScope{organization: org}, command},
			{"operator", nativeScope{organization: org, credential: apiCredential{ID: principal, Hash: principal, Scope: apiScopeOperator}}, command},
		} {
			t.Run(string(org)+"/"+test.name, func(t *testing.T) {
				if err := service.executeSpritePoolCommand(t.Context(), test.scope, test.command); err == nil {
					t.Fatal("refused command succeeded")
				}
			})
		}
		var revokeErr error
		details.afterStore = func(attachment.ArchiveReference) {
			details.afterStore = nil
			revokeErr = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.RevokeAccessCredential(t.Context(), principal, now) })
		}
		err = service.executeSpritePoolCommand(t.Context(), scope, spritePoolCommand{Mutation: tracker.Mutation{IdempotencyKey: "revoked-during-upload"}, spritePoolSettings: spritePoolSettings{Revision: 2, MinRunners: 1, MaxRunners: 5}, Bootstrap: new("printf changed")})
		details.afterStore = nil
		if revokeErr != nil || err == nil {
			t.Fatalf("authority changed during upload: revoke=%v mutation=%v", revokeErr, err)
		}
		if err := service.executeSpritePoolCommand(t.Context(), scope, command); err == nil {
			t.Fatal("revoked administrator replayed settings command")
		}
		if _, _, err := service.spritePoolSettingsRecord(t.Context(), scope); err == nil {
			t.Fatal("revoked administrator read pool settings")
		}
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
	for org, service := range services {
		service.config.TenantRepository, service.config.AccessRepository = reopened, reopened
		if err := reopened.ReadSprite(t.Context(), org, corestore.Serving, func(r postgres.SpriteReader) error {
			row, err := r.SpritePool(t.Context())
			if err != nil {
				return err
			}
			if row.Revision != 2 || row.MaxRunners != 4 || !row.ShareIdleCapacity || row.PlacementMode != "local_first" || row.TodoThreshold != 3 || row.OverflowSlots != 2 || row.BootstrapObjectKey == nil {
				t.Fatalf("reopened settings=%+v", row)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSpriteScaleDecision(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		input spriteScaleInput
		want  spriteScaleDecision
	}{
		{"two queued issues on an empty project", spriteScaleInput{Depth: 2, Ceiling: 2}, spriteScaleDecision{Create: 2}},
		{"free capacity absorbs queue", spriteScaleInput{Depth: 2, Free: 2, Count: 1, Ceiling: 2}, spriteScaleDecision{}},
		{"only unmet depth scales", spriteScaleInput{Depth: 3, Free: 1, Count: 1, Ceiling: 4}, spriteScaleDecision{Create: 2}},
		{"ceiling caps a burst", spriteScaleInput{Depth: 10, Count: 1, Ceiling: 2}, spriteScaleDecision{Create: 1}},
		{"ceiling already reached", spriteScaleInput{Depth: 2, Count: 2, Ceiling: 2}, spriteScaleDecision{}},
		{"floor keeps warm members without work", spriteScaleInput{Floor: 1, Ceiling: 2}, spriteScaleDecision{Create: 1}},
		{"pending bootstrap or wake absorbs demand", spriteScaleInput{Depth: 2, Pending: 2, Count: 2, Ceiling: 3}, spriteScaleDecision{}},
		{"idle pool scales to zero", spriteScaleInput{Count: 2, Idle: 2, Ceiling: 2}, spriteScaleDecision{Delete: 2}},
		{"floor survives idle deletion", spriteScaleInput{Count: 2, Idle: 2, Floor: 1, Ceiling: 2}, spriteScaleDecision{Delete: 1}},
		{"busy or recently idle members stay", spriteScaleInput{Count: 2, Ceiling: 2}, spriteScaleDecision{}},
		{"queued demand keeps needed idle capacity", spriteScaleInput{Depth: 1, Free: 1, Count: 1, Idle: 1, Ceiling: 1}, spriteScaleDecision{}},
		{"surplus idle capacity is removed", spriteScaleInput{Depth: 1, Free: 2, Count: 2, Idle: 2, Ceiling: 2}, spriteScaleDecision{Delete: 1}},
		{"disabled pool never provisions", spriteScaleInput{Depth: 2}, spriteScaleDecision{}},
		{"shared ceiling reduction overrides queued demand", spriteScaleInput{Depth: 3, Count: 2, Idle: 2, Floor: 1, Ceiling: 1}, spriteScaleDecision{Delete: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := decideSpriteScale(test.input); got != test.want {
				t.Fatalf("decision = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestSpritePoolPlacement(t *testing.T) {
	for _, test := range []struct {
		name, mode                   string
		depth, overflow, wantCreated int
		localFree                    bool
	}{
		{"local-first below threshold", "local_first", 2, 1, 0, false},
		{"local-first at threshold", "local_first", 3, 1, 1, false},
		{"local-first large queue has bounded overflow", "local_first", 15, 2, 2, false},
		{"local-first free local capacity", "local_first", 3, 1, 0, true},
		{"Sprites-only ignores free local", "sprites_only", 2, 0, 2, true},
		{"blended subtracts local slots", "blended", 3, 0, 2, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "")
			keys := f.service.config.SecretKeys
			hosted := f.service.config.Hosted
			f.service.config.SecretKeys = nil
			f.service.config.Hosted = nil
			local := placementFixtureRunner(t, f, "local-runner", "local", false, 1, "gpt-6.1-sol", "test-model")
			if !test.localFree {
				busy := f.create(t, "already running")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", local.redemption.Credential, providerClaim(local, busy, "busy")), http.StatusOK)
			}
			for i := 2; i < test.depth; i++ {
				f.create(t, fmt.Sprintf("extra %d", i))
			}
			placement := policy.Placement{Mode: test.mode, OverflowSlots: test.overflow}
			if test.mode == "local_first" {
				placement.TodoThreshold = 3
			}
			setPlacementFixture(t, f, placement, 2)
			f.service.config.SecretKeys = keys
			f.service.config.Hosted = hosted
			if err := f.service.scaleSpritePool(t.Context(), scope, true); err != nil {
				t.Fatal(err)
			}
			if len(provider.created) != test.wantCreated {
				t.Fatalf("created=%d want=%d", len(provider.created), test.wantCreated)
			}
		})
	}
}

type spritePoolProvider struct {
	mu               sync.Mutex
	f                nativeFixture
	token            string
	fail             string
	nameConflicts    int
	conflictStatus   int
	created, deleted []string
	runners          []runnerauth.Redemption
	manual           []runnerFixture
	checkpoint       int
	started          chan struct{}
	deleteStarted    chan struct{}
	releaseDelete    chan struct{}
	stage            func(string)
}

func newSpritePoolFixture(t *testing.T, failure string) (nativeFixture, nativeScope, *spritePoolProvider) {
	t.Helper()
	provider := &spritePoolProvider{fail: failure}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+spritesSecretSentinel {
			t.Error("provider did not receive the project token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v1/sprites" {
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"sprites":[]}`)
				return
			}
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			provider.mu.Lock()
			provider.created = append(provider.created, body.Name)
			provider.mu.Unlock()
			if provider.nameConflicts > 0 {
				provider.nameConflicts--
				if provider.stage != nil {
					provider.stage("create conflict")
				}
				w.WriteHeader(provider.conflictStatus)
				_, _ = io.WriteString(w, `{"error":"Sprite name already exists"}`)
				return
			}
			if provider.fail == "billing" || provider.fail == "invalid token" {
				status := http.StatusPaymentRequired
				if provider.fail == "invalid token" {
					status = http.StatusUnauthorized
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, spritesSecretSentinel)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"name": body.Name, "id": "resource-" + body.Name})
			return
		}
		if r.Method == http.MethodGet && !strings.Contains(r.URL.Path, "/exec") && !strings.Contains(r.URL.Path, "/services/") && !strings.HasSuffix(r.URL.Path, "/checkpoints") {
			name := strings.TrimPrefix(r.URL.Path, "/v1/sprites/")
			provider.mu.Lock()
			exists := slices.Contains(provider.created, name) && !slices.Contains(provider.deleted, name)
			provider.mu.Unlock()
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"name": name, "id": "resource-" + name})
			return
		}
		if r.Method == http.MethodDelete {
			provider.mu.Lock()
			provider.deleted = append(provider.deleted, strings.TrimPrefix(r.URL.Path, "/v1/sprites/"))
			first := len(provider.deleted) == 1
			provider.mu.Unlock()
			if provider.deleteStarted != nil && first {
				close(provider.deleteStarted)
				select {
				case <-provider.releaseDelete:
				case <-r.Context().Done():
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/checkpoint") {
			if provider.stage != nil {
				provider.stage("checkpoint")
			}
			provider.checkpoint++
			if provider.fail == "checkpoint" {
				_, _ = io.WriteString(w, `{"type":"error"}`)
			} else {
				_, _ = io.WriteString(w, `{"type":"complete","id":"fixture-baseline"}`)
			}
			return
		}
		if strings.Contains(r.URL.Path, "/services/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(1 << 20)
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session_info","session_id":"fixture-session"}`)); err != nil {
			t.Error(err)
			return
		}
		_, script, err := conn.Read(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		_, eof, err := conn.Read(r.Context())
		if err != nil || len(eof) != 1 || eof[0] != 4 {
			t.Errorf("stdin EOF = %v, error = %v", eof, err)
			return
		}
		if provider.stage != nil {
			provider.stage("exec")
		}
		registration := strings.ReplaceAll(string(script), `'"'"'`, "'")
		matches := regexp.MustCompile(`--token '([^']+)'`).FindStringSubmatch(registration)
		if len(matches) != 2 {
			t.Error("bootstrap has no enrollment token in stdin")
			return
		}
		enrollmentToken := matches[1]
		if strings.Contains(r.URL.RawQuery, enrollmentToken) {
			t.Error("fresh enrollment must travel only through stdin")
		}
		name := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/sprites/"), "/")[0]
		if err := provider.f.service.config.TenantRepository.ReadTenant(r.Context(), provider.f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
			owner, ok := reader.(postgres.SpriteReader)
			if !ok {
				return accessstore.ErrTransactionRequired
			}
			member, err := owner.SpriteMember(r.Context(), name)
			if err != nil {
				return err
			}
			enrollment, err := reader.RunnerEnrollment(r.Context(), member.EnrollmentID)
			if err != nil {
				return err
			}
			if enrollment.TokenHash != apikey.HashToken(enrollmentToken) {
				t.Error("stdin token does not belong to this Sprite reservation")
			}
			return nil
		}); err != nil {
			t.Error(err)
			return
		}
		if !strings.Contains(string(script), "https://raw.githubusercontent.com/digitaldrywood/detent/v0.117.99/scripts/sprite-runner-bootstrap.sh") || !strings.Contains(string(script), "--version 'v0.117.99'") {
			t.Error("bootstrap must use immutable source and the Hub's pinned runner release")
		}
		if provider.fail == "cancel" {
			close(provider.started)
			_, _, _ = conn.Read(r.Context())
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageBinary, append([]byte{1}, []byte("Customer setup started\nprivate-provider-secret\n")...))
		if provider.fail == "bootstrap" {
			_ = conn.Write(r.Context(), websocket.MessageBinary, []byte{3, 7})
			return
		}
		if provider.fail == "installer" {
			output := "Customer setup completed\nRunner bootstrap started\nInstalling PostgreSQL client " + provider.token + "\nE: Version '" + spritesSecretSentinel + "' for 'postgresql-client-17' was not found\nAuthorization: Bearer private-provider-secret\nBootstrap stopped. Last completed step: GitHub CLI, ripgrep and fd installed.\n"
			_ = conn.Write(r.Context(), websocket.MessageBinary, append([]byte{1}, []byte(output[:20])...))
			_ = conn.Write(r.Context(), websocket.MessageBinary, append([]byte{1}, []byte(output[20:])...))
			_ = conn.Write(r.Context(), websocket.MessageBinary, append([]byte{1}, []byte(strings.Repeat("E: Failed to fetch https://private-provider-secret\n", 1000))...))
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"exit","exit_code":100}`))
			return
		}
		binding := runnerauth.NewBinding()
		credential, err := apikey.GenerateToken()
		if err != nil {
			t.Error(err)
			return
		}
		tier := "native-trusted"
		if strings.Contains(registration, "--isolation-tier 'sandbox'") {
			tier = "sandbox"
		}
		if !strings.Contains(registration, "--isolation-tier '"+tier+"'") {
			t.Error("registration must carry pool access tier")
		}
		redemption := runnerauth.Redemption{IsolationTier: tier, Binding: binding, Credential: credential, Hostname: name, SpriteName: name, DisplayName: name, Capacity: 1, Version: "fixture"}
		response := performHubAPIRequest(t, provider.f.service, http.MethodPost, "/api/v2/organizations/"+string(provider.f.project.OrganizationID)+"/runner-enrollments/redeem", enrollmentToken, redemption)
		if response.Code != http.StatusCreated {
			t.Errorf("bootstrap enrollment = %d: %s", response.Code, response.Body.String())
			return
		}
		provider.mu.Lock()
		provider.runners = append(provider.runners, redemption)
		provider.mu.Unlock()
		if provider.stage != nil {
			provider.stage("registered")
		}
		_ = conn.Write(r.Context(), websocket.MessageBinary, append([]byte{1}, []byte("Customer setup completed\nRunner bootstrap completed\n")...))
		_ = conn.Write(r.Context(), websocket.MessageBinary, []byte{3, 0})
	})
	client := &http.Client{Transport: spritesTestTransport(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Upgrade") != "websocket" {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			return recorder.Result(), nil
		}
		local, remote := net.Pipe()
		server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
		go func() { _ = server.Serve(&spritePipeListener{conn: remote}) }()
		if err := request.Write(local); err != nil {
			_ = local.Close()
			return nil, err
		}
		reader := bufio.NewReader(local)
		response, err := http.ReadResponse(reader, request)
		if err != nil {
			_ = local.Close()
			return nil, err
		}
		response.Body = &spritePipeBody{Conn: local, reader: reader}
		return response, nil
	})}
	f, scope, _ := newSpriteWakeFixture(t, client, false, 0)
	storeOrganizationSpritesSecret(t, f, spritesSecretSentinel)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	f.create(t, "first")
	f.create(t, "second")
	f.service.spriteWakeWork.Wait()
	if failure == "installer" {
		provider.manual = enrollSpriteWakeRunners(t, f, time.Hour, "existing-one", "existing-two")
	}
	f.service.config.Hosted = &HostedConfig{PublicURL: "https://hub.example.test", OrganizationID: string(scope.organization)}
	f.service.config.Version = "v0.117.99"
	f.service.config.generateToken = func() (string, error) {
		token, err := apikey.GenerateToken()
		provider.token = token
		return token, err
	}
	provider.f = f
	scope = setPlacementFixture(t, f, policy.Placement{Mode: "blended"}, 2)
	if err := f.service.updateSpritePoolRecord(t.Context(), scope, spritePoolCommand{spritePoolSettings: spritePoolSettings{MaxRunners: 2, IdleSeconds: 300, Revision: 1}, Bootstrap: new("true")}); err != nil {
		t.Fatal(err)
	}

	return f, scope, provider
}

type spritePipeListener struct{ conn net.Conn }

func (l *spritePipeListener) Accept() (net.Conn, error) {
	if l.conn == nil {
		return nil, net.ErrClosed
	}
	conn := l.conn
	l.conn = nil
	return conn, nil
}

func (*spritePipeListener) Close() error   { return nil }
func (*spritePipeListener) Addr() net.Addr { return &net.TCPAddr{} }

type spritePipeBody struct {
	net.Conn
	reader *bufio.Reader
}

func (b *spritePipeBody) Read(data []byte) (int, error) { return b.reader.Read(data) }

func TestSpritePoolLifecycle(t *testing.T) {
	for _, test := range []struct {
		name, failure                            string
		wantCreated, wantDeleted, wantCheckpoint int
		heartbeat                                bool
		setup                                    *bool
		emptyBootstrap                           bool
		tier                                     string
		names                                    []string
	}{
		{"sandbox pool", "", 2, 2, 2, false, nil, false, "sandbox", nil},
		{"two independent connected members then idle deletion", "", 2, 2, 2, false, nil, false, "", nil},
		{"project heartbeat triggers idle cleanup", "", 2, 2, 2, true, nil, false, "", nil},
		{"bootstrap failure deletes only its member", "bootstrap", 1, 1, 0, false, nil, false, "", nil},
		{"installer failure retains bounded safe diagnostics", "installer", 1, 1, 0, false, nil, false, "", nil},
		{"checkpoint failure deletes enrolled member", "checkpoint", 1, 1, 1, false, nil, false, "", nil},
		{"billing failure is retained for onboarding", "billing", 1, 0, 0, false, nil, false, "", nil},
		{"token rejection is retained for onboarding", "invalid token", 1, 0, 0, false, nil, false, "", nil},
		{"empty bootstrap with declared setup", "", 2, 2, 2, false, new(true), true, "", nil},
		{"empty bootstrap without setup", "", 2, 2, 2, false, new(false), true, "", nil},
		{"legacy hex names survive reconciliation and retirement", "", 2, 2, 2, false, nil, false, "", []string{"detent-0123456789abcdef0123456789abcdef", "detent-fedcba9876543210fedcba9876543210"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, test.failure)
			provider.stage = func(stage string) {
				if stage == "registered" {
					assertSpritePostgresFacts(t, f)
				}
			}

			if test.tier != "" || test.emptyBootstrap {
				settings, _, err := f.service.spritePoolSettingsRecord(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				command := spritePoolCommand{spritePoolSettings: settings}
				if test.tier != "" {
					if err := f.service.database.postgres.WriteSprite(t.Context(), scope.organization, corestore.Serving, func(w postgres.SpriteWriter) error {
						row, err := w.SpritePool(t.Context())
						if err != nil {
							return err
						}
						row.IsolationTier = test.tier
						_, err = w.PutSpritePool(t.Context(), row, row.Revision)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					command.Revision++
				}
				if test.emptyBootstrap {
					command.Bootstrap = new("")
				}
				if test.emptyBootstrap {
					if err := f.service.updateSpritePoolRecord(t.Context(), scope, command); err != nil {
						t.Fatal(err)
					}
				}
			}
			var err error
			if test.names != nil {
				view, readErr := f.service.readSpritePool(t.Context(), scope)
				if readErr != nil {
					t.Fatal(readErr)
				}
				for _, name := range test.names {
					created, createErr := f.service.createPoolSpriteNamed(t.Context(), scope, view.spritePoolSettings, true, name)
					if !created || createErr != nil {
						t.Fatalf("legacy member: created=%t err=%v", created, createErr)
					}
				}
				if err = f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
					t.Fatal(err)
				}
			} else if test.failure == "" {
				f.service.wakeSpriteRunnersAfter(scope, json.RawMessage(`{"state":"Todo"}`))
				f.service.spriteWakeWork.Wait()
			} else {
				err = f.service.scaleSpritePool(t.Context(), scope, true)
			}
			if (err != nil) != (test.failure != "") {
				t.Fatalf("scale error = %v", err)
			}
			view, err := f.service.readSpritePool(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			for _, member := range view.Members {
				if test.names == nil && !strings.HasPrefix(member.Name, "sprite-") || !validSpritesSlug(member.Name) {
					t.Fatalf("invalid new member name %q", member.Name)
				}
				if strings.Contains(member.BootstrapLog, provider.token) || strings.Contains(member.BootstrapLog, "private-provider-secret") || strings.Contains(member.BootstrapLog, spritesSecretSentinel) {
					t.Fatal("secret appeared in the retained bootstrap log")
				}
				if test.failure == "" && (member.State != "enrolled" || member.RunnerID == "") {
					t.Fatalf("successful member = %+v", member)
				}
				if test.failure == "installer" {
					for _, want := range []string{"Runner installer: installing PostgreSQL client", "Runner installer: APT requested package version unavailable", "Runner installer stopped after: GitHub CLI, ripgrep and fd installed", "Bootstrap exited with code 100", "Bootstrap did not complete"} {
						if !strings.Contains(member.BootstrapLog, want) {
							t.Fatalf("missing %q in %s", want, member.BootstrapLog)
						}
					}
					if len(member.BootstrapLog) > 8500 {
						t.Fatalf("unbounded bootstrap log: %d bytes", len(member.BootstrapLog))
					}
				}
				if test.failure == "" {
					token, tokenErr := f.service.poolSpriteToken(t.Context(), scope, member.Name)
					if tokenErr != nil || string(token) != spritesSecretSentinel {
						t.Fatalf("member token lookup failed: %v", tokenErr)
					}
					clear(token)
					r, err := f.service.readRunnerRouting(t.Context(), scope, member.RunnerID)
					if err != nil || r.IsolationTier != view.IsolationTier {
						t.Fatalf("provisioned tier=%q pool=%q err=%v", r.IsolationTier, view.IsolationTier, err)
					}
				}
				if test.failure == "billing" && !strings.Contains(member.BootstrapLog, "billing enabled") || test.failure == "invalid token" && !strings.Contains(member.BootstrapLog, "replace it through") {
					t.Fatalf("missing safe failure guidance: %s", member.BootstrapLog)
				}
			}
			if test.setup != nil {
				for _, runner := range provider.runners {
					checks := runnerauth.LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", RunnerSetupDeclared: test.setup}
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(runner.MachineID)+"/heartbeat", runner.Credential, map[string]any{"display_name": runner.DisplayName, "capacity": runner.Capacity, "version": runner.Version, "sprite_name": runner.SpriteName, "local_checks": checks})
					requireNativeStatus(t, response, http.StatusOK)
				}
				f.service.spriteWakeWork.Wait()
			}
			view.Bootstrap = "private-saved-bootstrap"
			status, err := f.service.coordinatorSpritePoolStatus(t.Context(), scope, view, "/settings/integrations#sprites", scope.project)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "private-saved-bootstrap") || strings.Contains(string(encoded), spritesSecretSentinel) || strings.Contains(string(encoded), "private-provider-secret") {
				t.Fatal("onboarding status exposed private setup or credentials")
			}
			projection := status.(map[string]any)
			declaration := projection["runner_setup_declared"].(*bool)
			if (declaration == nil) != (test.setup == nil) || declaration != nil && *declaration != *test.setup {
				t.Fatalf("setup declaration = %v, want %v", declaration, test.setup)
			}
			wantConnected := 0
			if test.failure == "" {
				wantConnected = 2
			}
			if projection["connected_runners"] != wantConnected {
				t.Fatalf("connected=%v, want %d", projection["connected_runners"], wantConnected)
			}
			for _, member := range projection["members"].([]map[string]any) {
				if member["provider_readiness"] != "unavailable" {
					t.Fatal("enrollment without provider reports was claimed ready")
				}
			}
			if test.failure == "" {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
					items, err := w.WorkItems(t.Context(), scope.project, 0, 200)
					if err != nil {
						return err
					}
					for _, item := range items {
						item.Archived = true
						if _, err := w.UpdateWorkItem(t.Context(), item); err != nil {
							return err
						}
					}
					owner, ok := w.(postgres.SpriteWriter)
					if !ok {
						return accessstore.ErrTransactionRequired
					}
					members, err := owner.SpriteMembers(t.Context(), "", 200)
					if err != nil {
						return err
					}
					for _, member := range members {
						row := spriteMembershipRecord(member)
						row.IdleSince = f.service.config.now().Add(-time.Hour)
						if err := w.UpdateRunnerSpriteMembership(t.Context(), row); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if test.heartbeat {
					runner := provider.runners[0]
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(runner.MachineID)+"/heartbeat", runner.Credential, map[string]any{"display_name": runner.DisplayName, "capacity": runner.Capacity, "version": runner.Version, "sprite_name": runner.SpriteName})
					requireNativeStatus(t, response, http.StatusOK)
				} else {
					f.service.maintainSpritePools(t.Context())
				}
				f.service.spriteWakeWork.Wait()
			}
			if len(provider.created) != test.wantCreated || len(provider.deleted) != test.wantDeleted || provider.checkpoint != test.wantCheckpoint {
				t.Fatalf("provider created=%d deleted=%d checkpoint=%d", len(provider.created), len(provider.deleted), provider.checkpoint)
			}
			for _, runner := range provider.manual {
				for _, deleted := range provider.deleted {
					if deleted == runner.redemption.SpriteName {
						t.Fatal("failed provisioning deleted a preexisting Sprite")
					}
				}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential, map[string]any{"display_name": runner.redemption.Hostname, "capacity": 1, "version": "fixture"})
				requireNativeStatus(t, response, http.StatusOK)
			}
			view, err = f.service.readSpritePool(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			for _, member := range view.Members {
				if member.State != "deleted" {
					t.Fatalf("final member state = %s", member.State)
				}
			}
			if test.failure != "" {
				provider.fail = ""
				now := spriteRetryTimeFixture(t, f, scope)
				f.service.config.now = func() time.Time { return now }
				if err := f.service.scaleSpritePool(context.Background(), scope, true); err != nil {
					t.Fatalf("next native scale-up cannot replace failure: %v", err)
				}
				if len(provider.created) != 3 {
					t.Fatalf("next scale-up created %d total members, want 3", len(provider.created))
				}
			}
		})
	}
}

func TestSpritePoolNameCollisions(t *testing.T) {
	for _, test := range []struct {
		name      string
		conflicts int
		status    int
		stored    bool
		retired   bool
	}{
		{"free pair", 0, 0, false, false},
		{"deleted member name is reserved", 0, 0, true, false},
		{"provider bad request collision", 1, http.StatusBadRequest, false, false},
		{"provider conflict collision", 1, http.StatusConflict, false, false},
		{"retired conflict never deletes the existing Sprite", 1, http.StatusConflict, false, true},
		{"provider collisions get suffix", 5, http.StatusBadRequest, false, false},
		{"provider collisions are bounded", 10, http.StatusBadRequest, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "")
			provider.nameConflicts, provider.conflictStatus = test.conflicts, test.status
			if test.retired {
				provider.stage = func(stage string) {
					if stage == "create conflict" {
						name := provider.created[len(provider.created)-1]
						if err := f.service.retirePoolSpriteReservation(t.Context(), scope, name, "reservation retired\n", false); err != nil {
							t.Error(err)
						}
					}
				}
			}
			if test.stored {
				hosted := f.service.config.Hosted
				f.service.config.Hosted = nil
				historical := prepareRunner(t, f, runnerauth.Read)
				f.service.config.Hosted = hosted
				now := formatHubTime(f.service.config.now())
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO organization_sprite_members(organization_id,name,provider_organization,enrollment_id,state,idle_since,created_at) VALUES(?,?,'detent-test',?,'deleted',?,?)`, scope.organization, "sprite-bright-badger", historical.enrollment.ID, now, now); err != nil {
					t.Fatal(err)
				}
			}
			view, err := f.service.readSpritePool(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			if test.stored {
				created, err := f.service.createPoolSpriteNamed(t.Context(), scope, view.spritePoolSettings, true, "sprite-bright-badger")
				if created || !errors.Is(err, errSpriteNameTaken) || len(provider.created) != 0 {
					t.Fatalf("insert collision was not retried: created=%t err=%v", created, err)
				}
			}
			draws := 0
			names := spriteNameGenerator{intN: func(n int) int {
				value := draws / 2
				draws++
				return value % n
			}}
			for range 2 {
				created, err := f.service.createPoolSpriteWithNames(t.Context(), scope, view.spritePoolSettings, true, &names)
				if test.conflicts == 10 {
					if !created || !errors.Is(err, errSpriteNamesExhausted) || len(provider.created) != 10 {
						t.Fatalf("unbounded conflict handling: created=%t err=%v calls=%d", created, err, len(provider.created))
					}
					break
				}
				if !created || err != nil {
					t.Fatalf("provision after collision: created=%t err=%v", created, err)
				}
			}
			if len(provider.deleted) != 0 {
				t.Fatalf("deleted a provider-owned name: %v", provider.deleted)
			}
			seen := make(map[string]bool)
			for i, name := range provider.created {
				if seen[name] || test.stored && name == "sprite-bright-badger" {
					t.Fatalf("reused taken name %q", name)
				}
				seen[name] = true
				if i < test.conflicts {
					var state string
					var revoked bool
					if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT m.state,e.revoked_at IS NOT NULL FROM organization_sprite_members m JOIN runner_enrollments e ON e.id=m.enrollment_id WHERE m.organization_id=? AND m.name=?`, scope.organization, name).Scan(&state, &revoked); err != nil {
						t.Fatal(err)
					}
					if state != "deleted" || !revoked {
						t.Fatalf("conflicted reservation state=%q revoked=%t", state, revoked)
					}
				}
			}
			if test.conflicts != 10 {
				if len(provider.runners) != 2 || provider.checkpoint != 2 {
					t.Fatalf("usable members=%d checkpoints=%d, want two", len(provider.runners), provider.checkpoint)
				}
				if test.conflicts == 5 && strings.Count(provider.created[5], "-") != 3 {
					t.Fatalf("missing collision suffix: %q", provider.created[5])
				}
			}
		})
	}
}

func TestSpritePoolCapacity(t *testing.T) {
	for _, test := range []struct {
		name                            string
		runners                         int
		reports, paused, wake, separate bool
		wantFree, wantPending           int
		wantLimit                       *int
	}{
		{name: "unauthenticated enrollment is not ready", runners: 1},
		{name: "provider bounds reported local capacity", runners: 1, reports: true, wantFree: 1, wantLimit: new(1)},
		{name: "shared subscription is counted once", runners: 2, reports: true, wantFree: 1, wantLimit: new(1)},
		{name: "explicit independent accounts add capacity", runners: 2, reports: true, separate: true, wantFree: 2, wantLimit: new(2)},
		{name: "paused member contributes no ready capacity", runners: 1, reports: true, paused: true, wantLimit: new(1)},
		{name: "waking a paused member avoids creating a replacement", runners: 1, reports: true, paused: true, wake: true, wantPending: 1, wantLimit: new(0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, _ := newSpritePoolFixture(t, "")
			hosted := f.service.config.Hosted
			f.service.config.Hosted = nil
			for i := range test.runners {
				r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				r.redemption.SpriteName = r.redemption.Hostname
				r.enroll(t)
				if test.reports {
					report := capacityReport(f.service.config.now())
					report.Models = []string{"gpt-6.1-sol"}
					if test.separate {
						report.SharedAccountAlias = string(rune('a' + i))
					}
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.ReplaceRunnerProviderReports(t.Context(), r.binding.RunnerID, []providercapacity.Report{report})
					}); err != nil {
						t.Fatal(err)
					}
				}
				if test.paused {
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
						identity, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
						if err != nil {
							return err
						}
						identity.LastHeartbeatAt = f.service.config.now().Add(-time.Minute)
						return w.UpdateRunnerIdentity(t.Context(), identity)
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.service.config.Hosted = hosted
			if test.wake {
				if _, err := f.service.wakeSpriteRunners(t.Context(), scope, "Todo"); err != nil {
					t.Fatal(err)
				}
			}
			view, err := f.service.readSpritePool(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			input, _, err := f.service.spritePoolSnapshot(t.Context(), scope, view)
			if err != nil {
				t.Fatal(err)
			}
			if (input.CreateLimit == nil) != (test.wantLimit == nil) || input.CreateLimit != nil && *input.CreateLimit != *test.wantLimit {
				t.Fatalf("provider reservation limit=%v want=%v", input.CreateLimit, test.wantLimit)
			}
			if input.Free != test.wantFree || input.Pending != test.wantPending {
				t.Fatalf("capacity free=%d pending=%d, want free=%d pending=%d", input.Free, input.Pending, test.wantFree, test.wantPending)
			}
		})
	}
}

func TestSpritePoolFreshAuthority(t *testing.T) {
	for _, test := range []struct {
		name, change string
		wantError    bool
	}{
		{"removed project token", "secret", true},
		{"revoked configuring administrator", "actor", false},
		{"disabled pool", "disable", false},
		{"project boundary", "project", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "")
			if test.change == "project" {
				scope.organization = "org_foreign"
			} else {
				writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
					switch test.change {
					case "secret":
						return w.DeleteAccessSecret(t.Context(), "", flySpritesToken, accessstore.SecretAudit{OrganizationID: scope.organization, Actor: "fixture", Kind: flySpritesToken, Event: "delete", RecordedAt: f.service.config.now()})
					case "actor":
						return w.RevokeAccessCredential(t.Context(), scope.credential.ID, f.service.config.now())
					case "disable":
						owner := w.(postgres.SpriteWriter)
						pool, err := owner.SpritePool(t.Context())
						if err != nil {
							return err
						}
						pool.MaxRunners = 0
						_, err = owner.PutSpritePool(t.Context(), pool, pool.Revision)
						return err
					}
					return nil
				})
			}
			err := f.service.scaleSpritePool(t.Context(), scope, true)
			if (err != nil) != test.wantError {
				t.Fatalf("scale error=%v", err)
			}
			if len(provider.created) != 0 || len(provider.deleted) != 0 {
				t.Fatal("provider called without current project authority")
			}
		})
	}
}

func TestSpritePoolCancelledBootstrap(t *testing.T) {
	f, scope, provider := newSpritePoolFixture(t, "cancel")
	provider.started = make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- f.service.scaleSpritePool(ctx, scope, true) }()
	waitSpriteWake(t, provider.started)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled bootstrap succeeded")
		}
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("cancelled bootstrap did not join")
	}
	view, err := f.service.readSpritePool(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Members) != 1 || view.Members[0].State != "deleting" || len(provider.deleted) != 0 {
		t.Fatalf("cancelled lifecycle=%+v deleted=%d", view.Members, len(provider.deleted))
	}
	provider.fail = ""
	if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
		t.Fatal(err)
	}
	if len(provider.deleted) != 1 {
		t.Fatal("existing lifecycle did not delete the cancelled member")
	}
}

func TestSpritePoolDeletionBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, change string
		wantDeleted  int
		wantError    bool
		managed      bool
	}{
		{"active host lease survives", "busy", 1, false, false},
		{"updated floor is preserved", "floor", 1, false, false},
		{"idle threshold is respected", "recent", 0, false, false},
		{"expired interrupted bootstrap is deleted by the existing lifecycle", "expired", 2, false, false},
		{"token from another provider organization cannot delete members", "organization", 0, true, false},
		{"project host lease survives desired zero", "busy", 1, false, true},
		{"updated project desired count is preserved", "floor", 1, false, true},
		{"lower shared ceiling preserves a busy project member", "busy ceiling", 1, false, true},
		{"retiring members do not cause excess retirement", "retiring", 0, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "")
			if test.managed {
				setSpriteAllocationFixture(t, f, 2, 0)
			}
			if err := f.service.scaleSpritePool(t.Context(), scope, true); err != nil {
				t.Fatal(err)
			}
			archiveSpriteIssuesFixture(t, f, true)
			updateSpriteMembersFixture(t, f, func(member *runnerstore.SpriteMembership) { member.IdleSince = f.service.config.now().Add(-time.Hour) })
			if test.managed && test.change != "busy ceiling" && test.change != "retiring" {
				setSpriteAllocationFixture(t, f, 0, 1)
			}
			var err error
			switch test.change {
			case "busy", "busy ceiling":
				if test.change == "busy ceiling" {
					updateSpritePoolFixture(t, f, func(pool *postgres.SpritePool) { pool.MaxRunners = 0 })
				}
				now := formatHubTime(f.service.config.now())
				_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,created_at,updated_at) SELECT 'lease-busy',i.id,r.machine_id,'session-busy',?,?,?,?,? FROM issues i JOIN runner_identities r ON r.organization_id=i.organization_id WHERE i.project_id=? LIMIT 1`, formatHubTime(f.service.config.now().Add(time.Hour)), now, now, now, now, scope.project)
			case "floor":
				if test.managed {
					setSpriteAllocationFixture(t, f, 1, 2)
					break
				}
				_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE organization_sprite_pools SET min_runners=1 WHERE organization_id=?`, scope.organization)
			case "recent":
				_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE organization_sprite_members SET idle_since=? WHERE organization_id=?`, formatHubTime(f.service.config.now()), scope.organization)
			case "expired":
				_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE organization_sprite_members SET state='bootstrapping' WHERE organization_id=?`, scope.organization)
				if err == nil {
					_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE runner_enrollments SET expires_at=? WHERE id IN (SELECT enrollment_id FROM organization_sprite_members WHERE organization_id=?)`, formatHubTime(f.service.config.now().Add(-time.Minute)), scope.organization)
				}
			case "organization":
				_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE organization_secrets SET organization_slug='another-org' WHERE organization_id=?`, scope.organization)
			case "retiring":
				updateSpritePoolFixture(t, f, func(pool *postgres.SpritePool) { pool.MaxRunners = 1 })
				view, err := f.service.readSpritePool(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE organization_sprite_members SET state='deleting' WHERE organization_id=? AND name=?`, scope.organization, view.Members[0].Name); err != nil {
					t.Fatal(err)
				}
				if err := f.service.deletePoolSprite(t.Context(), scope, view.Members[1]); err != nil {
					t.Fatal(err)
				}
				created, err := f.service.createPoolSprite(t.Context(), scope, view.spritePoolSettings, true)
				if err != nil || created {
					t.Fatalf("retiring member released paid capacity: created=%t err=%v", created, err)
				}
				if len(provider.deleted) != 0 {
					t.Fatal("retired another member while the shared reduction was already reserved")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			err = f.service.scaleSpritePool(t.Context(), scope, false)
			if (err != nil) != test.wantError {
				t.Fatalf("deletion error=%v", err)
			}
			if len(provider.deleted) != test.wantDeleted {
				t.Fatalf("deleted=%d, want %d", len(provider.deleted), test.wantDeleted)
			}
		})
	}
}

func TestSpritePoolMutationDuringDeletion(t *testing.T) {
	f, scope, provider := newSpritePoolFixture(t, "")
	if err := f.service.scaleSpritePool(t.Context(), scope, true); err != nil {
		t.Fatal(err)
	}
	archiveSpriteIssuesFixture(t, f, true)
	updateSpriteMembersFixture(t, f, func(member *runnerstore.SpriteMembership) { member.IdleSince = f.service.config.now().Add(-time.Hour) })
	provider.deleteStarted = make(chan struct{})
	provider.releaseDelete = make(chan struct{})
	f.service.maintainSpritePools(t.Context())
	waitSpriteWake(t, provider.deleteStarted)
	archiveSpriteIssuesFixture(t, f, false)
	f.service.wakeSpriteRunnersAfter(scope, json.RawMessage(`{"state":"Todo"}`))
	close(provider.releaseDelete)
	f.service.spriteWakeWork.Wait()
	if len(provider.created) != 4 || len(provider.deleted) != 2 {
		t.Fatalf("coalesced mutation created=%d deleted=%d", len(provider.created), len(provider.deleted))
	}
	view, err := f.service.readSpritePool(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, member := range view.Members {
		if member.State == "enrolled" {
			active++
		}
	}
	if active != 2 {
		t.Fatalf("active members=%d, want 2", active)
	}
}

func TestSpritePoolSettingsValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		settings spritePoolSettings
		valid    bool
	}{
		{"enabled without bootstrap", spritePoolSettings{MinRunners: 2, MaxRunners: 2}, true},
		{"bootstrap byte limit", spritePoolSettings{MaxRunners: 1, Bootstrap: strings.Repeat("x", 65536)}, true},
		{"bootstrap too large", spritePoolSettings{MaxRunners: 1, Bootstrap: strings.Repeat("x", 65537)}, false},
		{"negative floor", spritePoolSettings{MinRunners: -1, MaxRunners: 1}, false},
		{"inverted bounds", spritePoolSettings{MinRunners: 2, MaxRunners: 1}, false},
		{"organization ceiling beyond project limit", spritePoolSettings{MaxRunners: 101}, true},
		{"idle too small", spritePoolSettings{MaxRunners: 1, IdleSeconds: 29}, false},
		{"idle too large", spritePoolSettings{MaxRunners: 1, IdleSeconds: 86401}, false},
		{"negative revision", spritePoolSettings{MaxRunners: 1, Revision: -1}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateSpritePoolSettings(&test.settings); (err == nil) != test.valid {
				t.Fatalf("validation error = %v, valid = %t", err, test.valid)
			}
		})
	}
}

func TestSpritePoolAccessSettings(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		tier   *string
		legacy bool
		status int
		want   string
	}{
		{"default", nil, false, http.StatusOK, "native-trusted"},
		{"full access", new("native-trusted"), false, http.StatusOK, "native-trusted"},
		{"sandbox refused", new("sandbox"), false, http.StatusUnprocessableEntity, "native-trusted"},
		{"legacy omitted", nil, true, http.StatusOK, "sandbox"},
		{"legacy sandbox refused", new("sandbox"), true, http.StatusUnprocessableEntity, "sandbox"},
		{"legacy switch approved", new("native-trusted"), true, http.StatusOK, "native-trusted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSpriteNativeFixture(t, Config{})
			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("authority=%d %v", status, err)
			}
			if _, err := f.service.spritePoolResponse(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}); err != nil {
				t.Fatal(err)
			}
			path := f.base + "/sprite-pool"
			var before spritePoolView
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil), &before)
			if test.legacy {
				response := performHubAPIRequest(t, f.service, http.MethodPut, path, testHubAdminToken, map[string]any{"min_runners": 0, "max_runners": 0, "revision": before.Revision})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &before)
				if err := f.service.database.postgres.WriteSprite(t.Context(), f.project.OrganizationID, corestore.Serving, func(w postgres.SpriteWriter) error {
					row, err := w.SpritePool(t.Context())
					if err != nil {
						return err
					}
					row.IsolationTier = "sandbox"
					_, err = w.PutSpritePool(t.Context(), row, row.Revision)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				before.Revision++
			}
			input := map[string]any{"min_runners": 0, "max_runners": 0, "revision": before.Revision}
			if test.tier != nil {
				input["isolation_tier"] = *test.tier
			}
			response := performHubAPIRequest(t, f.service, http.MethodPut, path, testHubAdminToken, input)
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusOK && !strings.Contains(response.Body.String(), "Sandbox on a Fly Sprite needs a passing isolation probe on that runner: update the runner, then switch it to Sandbox once it reports sandbox support.") {
				t.Fatalf("sandbox rejection = %s", response.Body.String())
			}
			var after spritePoolView
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil), &after)
			wantRevision := before.Revision
			if test.status == http.StatusOK {
				wantRevision++
			}
			if after.IsolationTier != test.want || after.Revision != wantRevision {
				t.Fatalf("pool = %+v, want tier %s revision %d", after, test.want, wantRevision)
			}
			if test.want == "sandbox" && after.IsolationTierUnavailableReason != "Sandbox on a Fly Sprite needs a passing isolation probe on that runner: update the runner, then switch it to Sandbox once it reports sandbox support." {
				t.Fatalf("legacy pool reason = %q", after.IsolationTierUnavailableReason)
			}
		})
	}
}

func setSpriteAllocationFixture(t *testing.T, f nativeFixture, desired int, revision tracker.Revision) projectSpriteAllocation {
	t.Helper()
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("fixture authority=%d %v", status, err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	raw, err := f.service.setSpriteAllocationRecord(t.Context(), scope, "fixture_sprite_allocation", projectSpriteAllocationRequest{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("allocation")}, ExpectedRevision: revision, DesiredCount: desired})
	if err != nil {
		t.Fatal(err)
	}
	var result projectSpriteAllocation
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func writeSpriteFixture(t *testing.T, f nativeFixture, update func(tenantstore.Writer) error) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, update); err != nil {
		t.Fatal(err)
	}
}

func updateSpritePoolFixture(t *testing.T, f nativeFixture, update func(*postgres.SpritePool)) {
	t.Helper()
	writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
		owner := w.(postgres.SpriteWriter)
		pool, err := owner.SpritePool(t.Context())
		if err != nil {
			return err
		}
		update(&pool)
		_, err = owner.PutSpritePool(t.Context(), pool, pool.Revision)
		return err
	})
}

func updateSpriteMembersFixture(t *testing.T, f nativeFixture, update func(*runnerstore.SpriteMembership)) {
	t.Helper()
	writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
		members, err := w.(postgres.SpriteReader).SpriteMembers(t.Context(), "", 200)
		if err != nil {
			return err
		}
		for _, member := range members {
			row := spriteMembershipRecord(member)
			update(&row)
			if err := w.UpdateRunnerSpriteMembership(t.Context(), row); err != nil {
				return err
			}
		}
		return nil
	})
}

func archiveSpriteIssuesFixture(t *testing.T, f nativeFixture, archived bool) {
	t.Helper()
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("archive authority=%d %v", status, err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	var items []corestore.WorkItem
	err = f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		items, err = r.WorkItems(t.Context(), scope.project, 0, 200)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Archived == archived {
			continue
		}
		if _, err := f.service.setNativeArchiveCommand(t.Context(), scope, string(item.NativeID), archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("sprite-archive")}, ExpectedRevision: tracker.Revision(item.Revision)}, archived); err != nil {
			t.Fatal(err)
		}
	}
}

func claimSpriteFixture(t *testing.T, f nativeFixture, runnerID, leaseID string) leasestore.Lease {
	t.Helper()
	var lease leasestore.Lease
	writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
		identity, err := w.RunnerIdentity(t.Context(), runnerID)
		if err != nil {
			return err
		}
		items, err := w.WorkItems(t.Context(), f.project.ID, 0, 2)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return corestore.ErrNotFound
		}
		item := items[0]
		archived := item.Archived
		if archived {
			item.Archived = false
			item, err = w.UpdateWorkItem(t.Context(), item)
			if err != nil {
				return err
			}
		}
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(identity.MachineID), SessionID: "session-" + leaseID, LeaseID: leaseID, TTL: time.Hour})
		if err != nil {
			return err
		}
		if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: f.project.OrganizationID, LeaseID: leaseID, RunnerID: runnerID, AssignedAt: lease.AcquiredAt}); err != nil {
			return err
		}
		if archived {
			item, err = w.WorkItem(t.Context(), f.project.ID, item.NativeID)
			if err != nil {
				return err
			}
			item.Archived = true
			_, err = w.UpdateWorkItem(t.Context(), item)
		}
		return err
	})
	return lease
}

func spriteRetryTimeFixture(t *testing.T, f nativeFixture, scope nativeScope) time.Time {
	t.Helper()
	view, err := f.service.readSpritePool(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	last := view.Members[len(view.Members)-1]
	var next time.Time
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		for _, phase := range []string{"provision", "bootstrap", "checkpoint"} {
			operation, err := r.(postgres.SpriteReader).SpriteOperation(t.Context(), phase+":"+last.EnrollmentID)
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if operation.Outcome == "rejected" && operation.NextAttemptAt.After(next) {
				next = operation.NextAttemptAt
			}
		}
		if next.IsZero() {
			return corestore.ErrNotFound
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return next
}

func spriteRunnerAllowedFixture(t *testing.T, f nativeFixture, scope nativeScope, runnerID string) (bool, error) {
	t.Helper()
	var allowed bool
	err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		allowed, err = r.(postgres.SpriteReader).SpriteRunnerAllowed(t.Context(), runnerID, scope.project, f.service.config.now())
		return err
	})
	return allowed, err
}

func setSpriteMemberChecks(t *testing.T, f nativeFixture, checks runnerauth.LocalChecks) {
	t.Helper()
	checks.ObservedAt = f.service.config.now()
	err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(postgres.SpriteReader)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		members, err := owner.SpritePoolMemberPage(t.Context(), postgres.SpriteMemberCursor{}, 200)
		if err != nil {
			return err
		}
		for _, member := range members {
			if tracker.ProjectID(member.RunnerSpriteMembership.AllocationProjectID) == f.project.ID && member.RunnerID != "" {
				if err := w.ReplaceRunnerLocalChecks(t.Context(), member.RunnerID, f.project.ID, checks); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

var spriteMemberReady = runnerauth.LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex"}}

func TestProjectSpriteAllocationCommands(t *testing.T) {
	var poolConfig *pgxpool.Config
	repository, _ := projectRecordPostgresWithAdmin(t, 4, func(cfg *pgxpool.Config) { poolConfig = cfg.Copy() })
	cfg := Config{SecretKeys: secretTestKeys(t, "1", "1"), RunDetails: &verifiedAttemptDetails{}}
	service := openLocalPostgresHubTestService(t, repository, cfg)
	service.registerSpritePoolRoutes(service.echo)
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: org, Name: string(org), Slug: string(org)})
	}
	f := newNativeFixture(t, service, "org_first", "first")
	setPlacementFixture(t, f, policy.Placement{Mode: "blended"}, 2)
	later := newNativeFixture(t, f.service, f.project.OrganizationID, "later")
	other := newNativeFixture(t, f.service, "org_first_second", "other")
	otherLater := newNativeFixture(t, f.service, other.project.OrganizationID, "other later")
	setPlacementFixture(t, other, policy.Placement{Mode: "blended"}, 101)
	fixtures := []nativeFixture{f, later, other, otherLater}
	for _, project := range fixtures {
		t.Run(string(project.project.ID), func(t *testing.T) {
			for _, test := range []struct {
				name, key, revision string
				desired, status     int
			}{
				{"initial count may exceed ceiling", "allocate", "0", 5, http.StatusOK},
				{"replay preserves revision", "allocate", "0", 5, http.StatusOK},
				{"key cannot change count", "allocate", "0", 6, http.StatusConflict},
				{"stale writes fail", "stale", "0", 6, http.StatusConflict},
				{"operator cannot allocate", "operator", "1", 0, http.StatusForbidden},
				{"negative count", "negative", "1", -1, http.StatusUnprocessableEntity},
			} {
				t.Run(test.name, func(t *testing.T) {
					token := testHubAdminToken
					if test.name == "operator cannot allocate" {
						token = project.token
					}
					response := performHubAPIRequest(t, f.service, http.MethodPut, project.base+"/sprite-pool/allocation", token, map[string]any{"idempotency_key": test.key + string(project.project.ID), "expected_revision": test.revision, "desired_count": test.desired})
					requireNativeStatus(t, response, test.status)
				})
			}
			for _, path := range []string{project.base + "/sprite-pool", project.base + "/sprite-pool/allocation"} {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, "", nil), http.StatusUnauthorized)
				foreign := other.token
				if project.project.OrganizationID == other.project.OrganizationID {
					foreign = f.token
				}
				refused := performHubAPIRequest(t, f.service, http.MethodGet, path, foreign, nil)
				if refused.Code != http.StatusUnauthorized && refused.Code != http.StatusForbidden {
					t.Fatalf("foreign tenant response=%d %s", refused.Code, refused.Body.String())
				}
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, project.base+"/sprite-pool/allocation", project.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var allocation projectSpriteAllocation
			decodeHubResponse(t, response, &allocation)
			if !allocation.Managed || allocation.Revision != 1 || allocation.DesiredCount != 5 || allocation.Shortfall != 5 || allocation.AllocatedCount != 0 {
				t.Fatalf("allocation = %+v", allocation)
			}
		})
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reopened, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	resumed := openLocalPostgresHubTestService(t, reopened, cfg)
	resumed.registerSpritePoolRoutes(resumed.echo)
	for _, project := range fixtures {
		response := performHubAPIRequest(t, resumed, http.MethodGet, project.base+"/sprite-pool/allocation", project.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var got projectSpriteAllocation
		decodeHubResponse(t, response, &got)
		if !got.Managed || got.DesiredCount != 5 || got.Revision != 1 || got.Shortfall != 5 {
			t.Fatalf("allocation after service reopen=%+v", got)
		}
		response = performHubAPIRequest(t, resumed, http.MethodPut, project.base+"/sprite-pool/allocation", testHubAdminToken, map[string]any{"idempotency_key": "allocate" + string(project.project.ID), "expected_revision": "0", "desired_count": 5})
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &got)
		if got.Revision != 1 {
			t.Fatalf("receipt replay after service reopen=%+v", got)
		}
		response = performHubAPIRequest(t, resumed, http.MethodGet, "/api/v2/organizations/"+string(project.project.OrganizationID)+"/sprite-pool", testHubAdminToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var settings spritePoolView
		decodeHubResponse(t, response, &settings)
		want := 2
		if project.project.OrganizationID == "org_first_second" {
			want = 101
		}
		if settings.MaxRunners != want {
			t.Fatalf("pool after service reopen=%+v", settings)
		}
	}

}

func TestSpritePoolOperatorTools(t *testing.T) {
	f := newSpriteNativeFixture(t, Config{})
	captured := make(chan context.Context, 1)
	f.service.echo.GET("/api/v2/organizations/:organization/sprite-tools-test", func(c echo.Context) error {
		captured <- c.Request().Context()
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	executor := hubProjectExecutor{f.service}
	connect := func(name, token string) context.Context {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/sprite-tools-test", token, nil), http.StatusOK)
		ctx := operatortool.BindConnection(<-captured, "sprite-tools-"+name, "sprite fixture")
		if err := executor.OpenConnection(ctx); err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	contexts := map[string]context.Context{"admin": connect("admin", testHubAdminToken), "operator": connect("operator", f.token)}
	project := string(f.project.ID)
	for _, test := range []struct {
		name, caller, tool, arguments string
		refusal                       error
		message                       string
		revision                      int64
		desired                       int
	}{
		{name: "organization pool read", caller: "admin", tool: operatortool.GetSpritePool, arguments: `{}`},
		{name: "operator cannot configure", caller: "operator", tool: operatortool.UpdateSpritePool, arguments: `{"project_id":"` + project + `","request_id":"operator","input":{"min_runners":0,"max_runners":0,"revision":0}}`, refusal: operatortool.ErrAccessDenied},
		{name: "sandbox refused with reason", caller: "admin", tool: operatortool.UpdateSpritePool, arguments: `{"request_id":"sandbox","input":{"min_runners":0,"max_runners":0,"revision":0,"isolation_tier":"sandbox"}}`, message: spriteSandboxUnavailableReason},
		{name: "configure organization pool", caller: "admin", tool: operatortool.UpdateSpritePool, arguments: `{"request_id":"configure","input":{"min_runners":0,"max_runners":0,"idle_seconds":600,"revision":0}}`, revision: 1},
		{name: "stale pool revision", caller: "admin", tool: operatortool.UpdateSpritePool, arguments: `{"request_id":"stale","input":{"min_runners":0,"max_runners":0,"revision":0}}`, refusal: mutation.ErrConflict},
		{name: "project pool read", caller: "operator", tool: operatortool.GetSpritePool, arguments: `{"project_id":"` + project + `"}`, revision: 1},
		{name: "operator cannot allocate", caller: "operator", tool: operatortool.UpdateProjectSpriteAllocation, arguments: `{"project_id":"` + project + `","request_id":"operator","input":{"expected_revision":"0","desired_count":2}}`, refusal: operatortool.ErrAccessDenied},
		{name: "allocate project Sprites", caller: "admin", tool: operatortool.UpdateProjectSpriteAllocation, arguments: `{"project_id":"` + project + `","request_id":"allocate","input":{"expected_revision":"0","desired_count":2}}`, revision: 1, desired: 2},
		{name: "project allocation read", caller: "operator", tool: operatortool.GetProjectSpriteAllocation, arguments: `{"project_id":"` + project + `"}`, revision: 1, desired: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := executor.Execute(contexts[test.caller], operatortool.Call{Name: test.tool, Arguments: json.RawMessage(test.arguments)})
			var request *operatortool.RequestError
			switch {
			case test.refusal != nil:
				if !errors.Is(err, test.refusal) {
					t.Fatalf("error = %v, want %v", err, test.refusal)
				}
				return
			case test.message != "":
				if !errors.As(err, &request) || request.Message != test.message {
					t.Fatalf("error = %v, want %q", err, test.message)
				}
				return
			case err != nil:
				t.Fatal(err)
			}
			raw := result.Content
			if test.tool == operatortool.UpdateSpritePool || test.tool == operatortool.UpdateProjectSpriteAllocation {
				var action chatpkg.Action
				if err := json.Unmarshal(raw, &action); err != nil || action.Status != chatpkg.ActionSucceeded {
					t.Fatalf("action = %s, %v", raw, err)
				}
				raw = json.RawMessage(action.Result)
			} else {
				var page struct {
					Data json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(raw, &page); err != nil {
					t.Fatal(err)
				}
				raw = page.Data
			}
			var got struct {
				Revision     json.Number `json:"revision"`
				DesiredCount int         `json:"desired_count"`
				IdleSeconds  int         `json:"idle_seconds"`
				Members      []any       `json:"members"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.Revision.String() != strconv.FormatInt(test.revision, 10) || got.DesiredCount != test.desired {
				t.Fatalf("%s = %s", test.tool, raw)
			}
			if test.tool == operatortool.GetSpritePool && test.revision == 1 && (got.IdleSeconds != 600 || got.Members == nil) {
				t.Fatalf("pool read did not return the saved settings view: %s", raw)
			}
		})
	}
	var browser spritePoolView
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/sprite-pool", testHubAdminToken, nil), &browser)
	if browser.Revision != 1 || browser.IdleSeconds != 600 {
		t.Fatalf("Settings view after MCP update = %+v", browser.spritePoolSettings)
	}
}

func TestProjectSpriteAllocationLifecycle(t *testing.T) {
	for _, order := range []string{"oldest", "project ID tie"} {
		t.Run(order, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "")
			hosted := f.service.config.Hosted
			f.service.config.Hosted = nil
			later := newNativeFixture(t, f.service, f.project.OrganizationID, "later")
			f.service.config.Hosted = hosted
			now := f.service.config.now()
			f.service.config.now = func() time.Time { return now }
			first, second := f, later
			if order == "oldest" {
				setSpriteAllocationFixture(t, later, 3, 0)
				now = now.Add(time.Hour)
				setSpriteAllocationFixture(t, f, 3, 0)
				first, second = later, f
			} else {
				setSpriteAllocationFixture(t, f, 3, 0)
				setSpriteAllocationFixture(t, later, 3, 0)
				if later.project.ID < f.project.ID {
					first, second = later, f
				}
			}
			f.service.maintainSpritePools(t.Context())
			f.service.spriteWakeWork.Wait()
			read := func(project nativeFixture) projectSpriteAllocation {
				t.Helper()
				allocation, err := f.service.projectSpriteAllocation(t.Context(), nativeScope{organization: scope.organization, project: project.project.ID, credential: scope.credential})
				if err != nil {
					t.Fatal(err)
				}
				return allocation
			}
			if a, b := read(first), read(second); a.AllocatedCount != 2 || a.Shortfall != 1 || b.AllocatedCount != 0 || b.Shortfall != 3 {
				t.Fatalf("first=%+v second=%+v", a, b)
			}
			if len(provider.created) != 2 {
				t.Fatalf("created=%d", len(provider.created))
			}
			setSpriteMemberChecks(t, first, spriteMemberReady)
			for _, runner := range provider.runners {
				for _, project := range []nativeFixture{first, second} {
					allowed, err := spriteRunnerAllowedFixture(t, f, nativeScope{organization: scope.organization, project: project.project.ID}, runner.RunnerID)
					if err != nil || allowed != (project.project.ID == first.project.ID) {
						t.Fatalf("claim project=%s allowed=%t err=%v", project.project.ID, allowed, err)
					}
				}
				identity, err := f.service.readRunnerRouting(t.Context(), nativeScope{organization: scope.organization, credential: scope.credential}, runner.RunnerID)
				if err != nil || identity.Scope != "projects" {
					t.Fatalf("runner scope=%s err=%v", identity.Scope, err)
				}
			}
			now = now.Add(time.Second)
			setSpriteAllocationFixture(t, first, 1, 1)
			updateSpriteMembersFixture(t, f, func(member *runnerstore.SpriteMembership) { member.IdleSince = f.service.config.now().Add(-time.Hour) })
			if err := f.service.scaleSpritePool(t.Context(), scope, true); err != nil {
				t.Fatal(err)
			}
			if a, b := read(first), read(second); a.AllocatedCount != 1 || b.AllocatedCount != 1 || b.Shortfall != 2 {
				t.Fatalf("after freeing first=%+v second=%+v", a, b)
			}
			if len(provider.created) != 3 || len(provider.deleted) != 1 {
				t.Fatalf("created=%d deleted=%d", len(provider.created), len(provider.deleted))
			}
			updateSpritePoolFixture(t, f, func(pool *postgres.SpritePool) { pool.MaxRunners = 1 })
			updateSpriteMembersFixture(t, f, func(member *runnerstore.SpriteMembership) { member.IdleSince = f.service.config.now().Add(-time.Hour) })
			f.service.maintainSpritePools(t.Context())
			f.service.spriteWakeWork.Wait()
			if a, b := read(first), read(second); a.AllocatedCount != 1 || b.AllocatedCount != 0 || b.Shortfall != 3 || a.DesiredCount != 1 || b.DesiredCount != 3 {
				t.Fatalf("lowered shared ceiling first=%+v second=%+v", a, b)
			}
			if len(provider.created) != 3 || len(provider.deleted) != 2 {
				t.Fatalf("lowered ceiling created=%d deleted=%d", len(provider.created), len(provider.deleted))
			}
		})
	}
}

func TestProjectSpriteBudgetSelection(t *testing.T) {
	for _, managed := range []bool{true, false} {
		name := "eligible organization demand"
		if managed {
			name = "eligible project request"
		}
		t.Run(name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "")
			hosted, keys := f.service.config.Hosted, f.service.config.SecretKeys
			f.service.config.Hosted, f.service.config.SecretKeys = nil, nil
			later := newNativeFixture(t, f.service, f.project.OrganizationID, "later")
			now := f.service.config.now()
			f.service.config.now = func() time.Time { return now }
			setSpriteAllocationFixture(t, f, 2, 0)
			setMonthlyBudgetFixture(t, f, false, budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, SpriteMicros: new(int64)})
			now = now.Add(time.Second)
			if managed {
				setSpriteAllocationFixture(t, later, 2, 0)
			} else {
				approveHubTestPolicy(t, f.service, later.base+"/policy", hubTestPolicy())
				later.create(t, "later first")
				later.create(t, "later second")
			}

			f.service.config.Hosted, f.service.config.SecretKeys = hosted, keys
			if err := f.service.scaleSpritePool(t.Context(), scope, true); err != nil {
				t.Fatal(err)
			}
			oldest, err := f.service.projectSpriteAllocation(t.Context(), scope)
			if err != nil || oldest.AllocatedCount != 0 || oldest.Shortfall != 2 {
				t.Fatalf("budget-blocked allocation=%+v err=%v", oldest, err)
			}
			view, err := f.service.readSpritePool(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			owner := tracker.ProjectID("")
			if managed {
				owner = later.project.ID
			}
			for _, member := range view.Members {
				if member.AllocationProjectID != owner {
					t.Fatalf("member allocated to %q, want %q", member.AllocationProjectID, owner)
				}
			}
			if len(provider.created) != 2 || len(provider.deleted) != 0 {
				t.Fatalf("created=%d deleted=%d", len(provider.created), len(provider.deleted))
			}
		})
	}
}

func TestProjectSpriteConcurrentReservations(t *testing.T) {
	f, scope, provider := newSpritePoolFixture(t, "")
	setSpriteAllocationFixture(t, f, 4, 0)
	writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
		owner := w.(postgres.SpriteWriter)
		pool, err := owner.SpritePool(t.Context())
		if err != nil {
			return err
		}
		pool.MaxRunners = 1
		_, err = owner.PutSpritePool(t.Context(), pool, pool.Revision)
		return err
	})
	start := make(chan struct{})
	results := make(chan error, 4)
	for range 4 {
		go func() { <-start; results <- f.service.scaleSpritePool(t.Context(), scope, true) }()
	}
	close(start)
	for range 4 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if len(provider.created) != 1 {
		t.Fatalf("concurrent reservations created %d", len(provider.created))
	}
	allocation := setSpriteAllocationFixture(t, f, 0, 1)
	if allocation.AllocatedCount != 1 || allocation.DesiredCount != 0 {
		t.Fatalf("allocation=%+v", allocation)
	}
	for _, runner := range provider.runners {
		allowed, err := spriteRunnerAllowedFixture(t, f, scope, runner.RunnerID)
		if err != nil || allowed {
			t.Fatalf("desired zero allowed=%t err=%v", allowed, err)
		}
	}
}

func TestProjectSpriteAllocationLimits(t *testing.T) {
	for _, test := range []struct {
		name                                              string
		organizationMembers, providerLimit, wantAllocated int
	}{
		{"organization allocations retain the shared ceiling", 2, 0, 1},
		{"reported provider limit includes every reservation", 0, 2, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "")
			updateSpritePoolFixture(t, f, func(pool *postgres.SpritePool) {
				pool.MinRunners, pool.MaxRunners = int32(test.organizationMembers), 3
			})
			if test.organizationMembers > 0 {
				if err := f.service.scaleSpritePool(t.Context(), scope, true); err != nil {
					t.Fatal(err)
				}
			}
			var manual runnerFixture
			if test.providerLimit > 0 {
				hosted, keys := f.service.config.Hosted, f.service.config.SecretKeys
				f.service.config.Hosted, f.service.config.SecretKeys = nil, nil
				manual = placementFixtureRunner(t, f, "manual-sprite", "sprite", true, test.providerLimit, "gpt-6.1-sol")
				f.service.config.Hosted, f.service.config.SecretKeys = hosted, keys
			}
			setSpriteAllocationFixture(t, f, 4, 0)
			if err := f.service.scaleSpritePool(t.Context(), scope, true); err != nil {
				t.Fatal(err)
			}
			allocation, err := f.service.projectSpriteAllocation(t.Context(), scope)
			if err != nil || allocation.AllocatedCount != test.wantAllocated || allocation.Shortfall != 4-test.wantAllocated {
				t.Fatalf("allocation=%+v err=%v", allocation, err)
			}
			view, err := f.service.readSpritePool(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			organizationCount := 0
			for _, member := range view.Members {
				if member.AllocationProjectID == "" && member.State != "deleted" {
					organizationCount++
				}
			}
			if organizationCount != test.organizationMembers {
				t.Fatalf("organization allocation changed to %d", organizationCount)
			}
			if test.providerLimit > 0 {
				allowed, err := spriteRunnerAllowedFixture(t, f, scope, manual.binding.RunnerID)
				if err != nil || !allowed {
					t.Fatalf("manual runner allowed=%t err=%v", allowed, err)
				}
			}
			if len(provider.deleted) != 0 {
				t.Fatal("scaling a project retired a different allocation")
			}
		})
	}
}

func TestProjectSpriteWorkspaceClaims(t *testing.T) {
	for _, test := range []struct {
		name, mode, state                   string
		desired                             int
		owned, sprite, other, unready, pool bool
		status                              int
	}{
		{name: "zero holds managed Sprite workspace jobs", desired: 0, owned: true, sprite: true, status: http.StatusConflict},
		{name: "blended local workspace jobs", desired: 2, status: http.StatusOK},
		{name: "blended zero permits local workspace jobs", status: http.StatusOK},
		{name: "local-first local workspace jobs", mode: "local_first", desired: 2, status: http.StatusOK},
		{name: "local-first zero permits local workspace jobs", mode: "local_first", status: http.StatusOK},
		{name: "Sprites-only excludes local workspace jobs", mode: "sprites_only", desired: 2, status: http.StatusConflict},
		{name: "Sprites-only zero excludes local workspace jobs", mode: "sprites_only", status: http.StatusConflict},
		{name: "manual Sprites can claim workspace jobs", desired: 2, sprite: true, status: http.StatusOK},
		{name: "manual Sprites can claim workspace jobs at zero desired", sprite: true, status: http.StatusOK},
		{name: "owned members can claim workspace jobs", desired: 1, owned: true, sprite: true, status: http.StatusOK},
		{name: "owned member without marker retains readiness checks", desired: 1, owned: true, unready: true, status: http.StatusConflict},
		{name: "owned member without marker retains desired count checks", owned: true, status: http.StatusConflict},
		{name: "unready owned member", desired: 1, owned: true, sprite: true, unready: true, status: http.StatusConflict},
		{name: "retiring owned member", desired: 1, owned: true, sprite: true, state: "deleting", status: http.StatusConflict},
		{name: "other project member excludes managed project", desired: 1, owned: true, sprite: true, other: true, status: http.StatusConflict},
		{name: "other project member excludes unmanaged project", desired: -1, owned: true, sprite: true, other: true, status: http.StatusConflict},
		{name: "other project member without marker", desired: 1, owned: true, other: true, status: http.StatusConflict},
		{name: "organization pool member excludes allocated project", desired: 2, owned: true, sprite: true, pool: true, status: http.StatusConflict},
		{name: "organization pool member excludes zero allocation", owned: true, sprite: true, pool: true, status: http.StatusConflict},
		{name: "organization pool member retains unmanaged project eligibility", desired: -1, owned: true, sprite: true, pool: true, status: http.StatusOK},
	} {
		for _, typed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/typed=%t", test.name, typed), func(t *testing.T) {
				f := newWorkspaceRunnerFixture(t)
				mode := test.mode
				if mode == "" {
					mode = "blended"
				}
				placement := policy.Placement{Mode: mode}
				if mode == "local_first" {
					placement.TodoThreshold, placement.OverflowSlots = 3, 2
				}
				setPlacementFixture(t, f.nativeFixture, placement, 2)
				if test.desired >= 0 {
					setSpriteAllocationFixture(t, f.nativeFixture, test.desired, 0)
				}
				if test.sprite {
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE machines SET machine_facts_ready=NULL,capabilities_json=json_set(capabilities_json,'$.sprite_name',hostname) WHERE id=?`, f.runner.binding.MachineID); err != nil {
						t.Fatal(err)
					}
				}
				if test.owned {
					owner := f.nativeFixture
					if test.other {
						owner = newNativeFixture(t, f.service, f.project.OrganizationID, "other")
						setSpriteAllocationFixture(t, owner, 1, 0)
					}
					state := test.state
					if state == "" {
						state = "enrolled"
					}
					allocationProject := owner.project.ID
					if test.pool {
						allocationProject = ""
					}
					now := formatHubTime(f.service.config.now())
					if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO organization_sprite_members(organization_id,allocation_project_id,name,provider_organization,enrollment_id,state,idle_since,created_at) VALUES(?,?,?,'test',?,?,?,?)`, f.project.OrganizationID, allocationProject, f.runner.redemption.Hostname, f.runner.enrollment.ID, state, now, now); err != nil {
						t.Fatal(err)
					}
					if !test.unready {
						setSpriteMemberChecks(t, owner, spriteMemberReady)
					}
				}
				setSpriteFactsFixtureMode(t, f.nativeFixture, typed)
				session := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
				item := f.item(t, session.ID)
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.runner.redemption.Credential, tracker.NativeClaim{PolicyID: f.policy, WorkItemID: tracker.NativeWorkItemID(item), MachineID: f.runner.binding.MachineID, SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}})
				requireNativeStatus(t, response, test.status)
			})
		}
	}
}

func TestHostedProjectSpriteAllocationAuthority(t *testing.T) {
	f := newSharedRouteFixture(t, func(cfg *Config) { cfg.RunDetails = &verifiedAttemptDetails{} })
	for org, service := range f.services {
		triggered := make(chan nativeScope, 3)
		service.startSpritePoolForQueue = func(scope nativeScope) { triggered <- scope }
		organization := tracker.OrganizationID(org)
		for _, project := range []tracker.ProjectID{"first", "second"} {
			if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				return w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: org + "_owner", ProjectID: project, CanWrite: true})
			}); err != nil {
				t.Fatal(err)
			}
		}
		path := "/api/v2/organizations/" + org + "/sprite-pool"
		response := f.request(t, org, path, `{"idempotency_key":"pool","revision":0,"min_runners":0,"max_runners":101}`, func(c *cloudassert.Claims) { sharedRouteBrowserClaims(c); c.Method = http.MethodPut }, map[string]string{"X-CSRF-Token": strings.Repeat("b", 64)})
		requireNativeStatus(t, response, http.StatusOK)
		for _, project := range []tracker.ProjectID{"first", "second"} {
			path := "/api/v2/organizations/" + org + "/projects/" + string(project) + "/sprite-pool/allocation"
			response := f.request(t, org, path, `{"idempotency_key":"allocate","expected_revision":"0","desired_count":2}`, func(c *cloudassert.Claims) { sharedRouteBrowserClaims(c); c.Method = http.MethodPut }, map[string]string{"X-CSRF-Token": strings.Repeat("b", 64)})
			requireNativeStatus(t, response, http.StatusOK)
			scope := nativeScope{organization: organization, project: project}
			allowed, err := service.projectSpriteAllocationAuthority(t.Context(), nil, scope)
			if err != nil || !allowed {
				t.Fatalf("configuring owner allowed=%t err=%v", allowed, err)
			}
			if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: org + "_owner", ProjectID: project, CanWrite: false})
			}); err != nil {
				t.Fatal(err)
			}
			allowed, err = service.projectSpriteAllocationAuthority(t.Context(), nil, scope)
			if err != nil || allowed {
				t.Fatalf("revoked project grant allowed=%t err=%v", allowed, err)
			}
		}
		if len(triggered) != 3 {
			t.Fatalf("lifecycle triggers=%d", len(triggered))
		}
		for range 3 {
			if got := <-triggered; got.organization != organization {
				t.Fatalf("foreign lifecycle trigger=%+v", got)
			}
		}
	}
}

func projectSpriteMembers(t *testing.T, f nativeFixture, scope nativeScope) []spritePoolMember {
	t.Helper()
	view, err := f.service.readSpritePool(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.describeSpritePoolMembers(t.Context(), scope, view.Members); err != nil {
		t.Fatal(err)
	}
	var members []spritePoolMember
	for _, member := range view.Members {
		if member.AllocationProjectID == f.project.ID && member.State != "deleted" {
			members = append(members, member)
		}
	}
	return members
}

func TestProjectSpriteScaleSequence(t *testing.T) {
	f, scope, provider := newSpritePoolFixture(t, "")
	updateSpritePoolFixture(t, f, func(pool *postgres.SpritePool) { pool.MaxRunners = 3 })
	revision := tracker.Revision(0)
	for _, step := range []struct{ desired, created, deleted int }{{1, 1, 0}, {3, 3, 0}, {1, 3, 2}, {0, 3, 3}, {2, 5, 3}} {
		revision = setSpriteAllocationFixture(t, f, step.desired, revision).Revision
		if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
			t.Fatal(err)
		}
		members := projectSpriteMembers(t, f, scope)
		for _, member := range members {
			if member.State != "enrolled" || member.Status != "awaiting_login" {
				t.Fatalf("desired %d left member %+v", step.desired, member)
			}
		}
		if len(members) != step.desired || len(provider.created) != step.created || len(provider.deleted) != step.deleted {
			t.Fatalf("desired %d: members=%d created=%d deleted=%d", step.desired, len(members), len(provider.created), len(provider.deleted))
		}
	}
	seen := map[string]bool{}
	for _, name := range provider.created {
		if seen[name] {
			t.Fatalf("duplicate member %s", name)
		}
		seen[name] = true
	}
}

func TestProjectSpriteLateProvisioning(t *testing.T) {
	for _, test := range []struct {
		stage      string
		checkpoint int
	}{
		{"registered", 0},
		{"checkpoint", 1},
	} {
		t.Run(test.stage, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "")
			setSpriteAllocationFixture(t, f, 1, 0)
			provider.stage = func(stage string) {
				if stage == test.stage {
					setSpriteAllocationFixture(t, f, 0, 1)
				}
			}
			if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
				t.Fatal(err)
			}
			provider.stage = nil
			if len(provider.created) != 1 || len(provider.deleted) != 1 || provider.checkpoint != test.checkpoint || len(provider.runners) != 1 {
				t.Fatalf("created=%d deleted=%d checkpoint=%d runners=%d", len(provider.created), len(provider.deleted), provider.checkpoint, len(provider.runners))
			}
			if members := projectSpriteMembers(t, f, scope); len(members) != 0 {
				t.Fatalf("late completion retained %+v", members)
			}
			setSpriteAllocationFixture(t, f, 1, 2)
			setSpriteMemberChecks(t, f, spriteMemberReady)
			allowed, err := spriteRunnerAllowedFixture(t, f, scope, provider.runners[0].RunnerID)
			if err != nil || allowed {
				t.Fatalf("late member allowed=%t err=%v", allowed, err)
			}
			allocation, err := f.service.projectSpriteAllocation(t.Context(), scope)
			if err != nil || allocation.ProvisionFailures != 0 {
				t.Fatalf("stale reservation counted as failure: %+v err=%v", allocation, err)
			}
		})
	}
}

func TestProjectSpriteDrain(t *testing.T) {
	for _, test := range []struct {
		name    string
		expired bool
		retire  func(t *testing.T, f nativeFixture, runner string)
	}{
		{name: "active work publishes before deletion", retire: func(t *testing.T, f nativeFixture, _ string) {
			writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
				lease, err := w.Lease(t.Context(), "lease-drain")
				if err != nil {
					return err
				}
				_, err = w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "completed")
				return err
			})
		}},
		{name: "expired lease on an offline member waits for audited retirement", expired: true, retire: func(t *testing.T, f nativeFixture, runner string) {
			hosted := f.service.config.Hosted
			f.service.config.Hosted = nil
			response := performHubAPIRequest(t, f.service, http.MethodDelete, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/runners/"+runner, testHubAdminToken, nil)
			f.service.config.Hosted = hosted
			requireNativeStatus(t, response, http.StatusNoContent)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "")
			setSpriteAllocationFixture(t, f, 1, 0)
			if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
				t.Fatal(err)
			}
			setSpriteMemberChecks(t, f, spriteMemberReady)
			member := projectSpriteMembers(t, f, scope)[0]
			lease := claimSpriteFixture(t, f, member.RunnerID, "lease-drain")
			if test.expired {
				now := lease.ExpiresAt.Add(time.Minute)
				f.service.config.now = func() time.Time { return now }
				writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), member.RunnerID)
					if err != nil {
						return err
					}
					identity.LastHeartbeatAt = now.Add(-time.Hour)
					return w.UpdateRunnerIdentity(t.Context(), identity)
				})
			}
			setSpriteAllocationFixture(t, f, 0, 1)
			for range 2 {
				if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
					t.Fatal(err)
				}
			}
			members := projectSpriteMembers(t, f, scope)
			if len(members) != 1 || members[0].State != "deleting" || members[0].Status != "draining" || len(provider.deleted) != 0 {
				t.Fatalf("draining members=%+v deleted=%d", members, len(provider.deleted))
			}
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				operation, err := r.(postgres.SpriteReader).SpriteOperation(t.Context(), "delete:"+member.EnrollmentID)
				if err != nil {
					return err
				}
				if operation.Outcome != "pending" || operation.Attempts != 0 || operation.ProviderResourceID == "" {
					t.Errorf("lease-held deletion attempted provider work: %+v", operation)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			runner, err := f.service.readRunnerRouting(t.Context(), scope, member.RunnerID)
			if err != nil || runner.State != "draining" {
				t.Fatalf("runner state=%q err=%v", runner.State, err)
			}
			test.retire(t, f, member.RunnerID)
			if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
				t.Fatal(err)
			}
			if members := projectSpriteMembers(t, f, scope); len(members) != 0 || len(provider.deleted) != 1 {
				t.Fatalf("after durable recovery members=%+v deleted=%d", members, len(provider.deleted))
			}
		})
	}
}

func TestProjectSpriteProvisioningRecovery(t *testing.T) {
	for _, test := range []struct {
		name    string
		rewrite bool
		resume  bool
		created int
	}{
		{"failed provisioning waits for its growing backoff", false, false, spriteProvisionAttempts},
		{"a rewrite during an attempt keeps every attempt for the new request", true, false, spriteProvisionAttempts + 1},
		{"same source resumes after its due time", false, true, spriteProvisionAttempts},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "bootstrap")
			now := f.service.config.now()
			f.service.config.now = func() time.Time { return now }
			allocation := setSpriteAllocationFixture(t, f, 1, 0)
			if test.rewrite {
				var rewrite sync.Once
				provider.stage = func(stage string) {
					if stage == "exec" {
						rewrite.Do(func() { setSpriteAllocationFixture(t, f, 1, allocation.Revision) })
					}
				}
				_ = f.service.scaleSpritePool(t.Context(), scope, false)
				provider.stage = nil
				rewritten, err := f.service.projectSpriteAllocation(t.Context(), scope)
				if err != nil || rewritten.Revision != 2 || rewritten.ProvisionFailures != 0 {
					t.Fatalf("old attempt charged the rewritten request: %+v err=%v", rewritten, err)
				}
			}
			for attempt := range spriteProvisionAttempts {
				if attempt > 0 {
					now = spriteRetryTimeFixture(t, f, scope)
				}
				if err := f.service.scaleSpritePool(t.Context(), scope, false); err == nil {
					t.Fatal("fixture bootstrap rejection was not reported")
				}
			}
			for range 2 {
				if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
					t.Fatal(err)
				}
			}
			provider.fail = ""
			if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
				t.Fatal(err)
			}
			held, err := f.service.projectSpriteAllocation(t.Context(), scope)
			if err != nil || held.ProvisionFailures != spriteProvisionAttempts || len(provider.created) != test.created || len(provider.deleted) != test.created {
				t.Fatalf("allocation=%+v created=%d deleted=%d err=%v", held, len(provider.created), len(provider.deleted), err)
			}
			if test.resume {
				now = spriteRetryTimeFixture(t, f, scope)
			} else {
				setSpriteAllocationFixture(t, f, 1, held.Revision)
			}
			if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
				t.Fatal(err)
			}
			if members := projectSpriteMembers(t, f, scope); len(members) != 1 || members[0].State != "enrolled" || len(provider.created) != test.created+1 {
				t.Fatalf("retry after new request members=%+v created=%d", members, len(provider.created))
			}
		})
	}
	t.Run("Hub restart releases an interrupted reservation", func(t *testing.T) {
		f, scope, provider := newSpritePoolFixture(t, "")
		setSpriteAllocationFixture(t, f, 1, 0)
		if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
			t.Fatal(err)
		}
		interrupted := projectSpriteMembers(t, f, scope)[0]
		updateSpriteMembersFixture(t, f, func(member *runnerstore.SpriteMembership) {
			if member.Name == interrupted.Name {
				member.State = "bootstrapping"
			}
		})
		now := f.service.config.now().Add(time.Hour)
		f.service.config.now = func() time.Time { return now }
		if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
			t.Fatal(err)
		}
		members := projectSpriteMembers(t, f, scope)
		if len(members) != 1 || members[0].Name == interrupted.Name || members[0].State != "enrolled" || len(provider.created) != 2 || len(provider.deleted) != 1 || provider.deleted[0] != interrupted.Name {
			t.Fatalf("members=%+v created=%v deleted=%v", members, provider.created, provider.deleted)
		}
		allocation, err := f.service.projectSpriteAllocation(t.Context(), scope)
		if err != nil || allocation.ProvisionFailures != 1 {
			t.Fatalf("allocation=%+v err=%v", allocation, err)
		}
	})
}

func TestProjectSpriteLoginReadiness(t *testing.T) {
	for _, test := range []struct {
		name         string
		checks       *runnerauth.LocalChecks
		heartbeatAgo time.Duration
		wakeFailed   bool
		allowed      bool
		status       string
		commands     []string
	}{
		{name: "no setup observation", status: "awaiting_login", commands: []string{"codex login --device-auth", "gh auth login", "gh auth setup-git"}},
		{name: "provider sign-in pending", checks: &runnerauth.LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "failed", ProviderKinds: []string{"claude_code"}}, status: "awaiting_login", commands: []string{"claude auth login"}},
		{name: "GitHub sign-in pending", checks: &runnerauth.LocalChecks{Checkout: "failed", Doctor: "pending", Provider: "passed", ProviderKinds: []string{"codex"}}, status: "awaiting_login", commands: []string{"gh auth login", "gh auth setup-git"}},
		{name: "doctor failure after logins", checks: &runnerauth.LocalChecks{Checkout: "passed", Doctor: "failed", Provider: "passed", ProviderKinds: []string{"codex"}}, status: "awaiting_setup"},
		{name: "verified logins", checks: &spriteMemberReady, allowed: true, status: "ready"},
		{name: "Pi backend without authentication status", checks: &runnerauth.LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "pending", ProviderKinds: []string{"pi_agent"}}, allowed: true, status: "ready"},
		{name: "verified sleeping member", checks: &spriteMemberReady, heartbeatAgo: time.Minute, allowed: true, status: "sleeping"},
		{name: "failed wake is unreachable", checks: &spriteMemberReady, heartbeatAgo: time.Minute, wakeFailed: true, allowed: true, status: "unreachable"},
	} {
		for _, typed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/typed=%t", test.name, typed), func(t *testing.T) {
				f, scope, provider := newSpritePoolFixture(t, "")
				setSpriteAllocationFixture(t, f, 1, 0)
				if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
					t.Fatal(err)
				}
				runner := provider.runners[0]
				if test.checks != nil {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(runner.MachineID)+"/heartbeat", runner.Credential, map[string]any{"display_name": runner.DisplayName, "capacity": runner.Capacity, "version": runner.Version, "sprite_name": runner.SpriteName, "local_checks": test.checks})
					requireNativeStatus(t, response, http.StatusOK)
					f.service.spriteWakeWork.Wait()
				}
				heartbeat := f.service.config.now().Add(-test.heartbeatAgo)
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET last_heartbeat_at=? WHERE machine_id=?`, formatHubTime(heartbeat), runner.MachineID); err != nil {
					t.Fatal(err)
				}
				if test.wakeFailed {
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE machines SET machine_facts_ready=NULL,capabilities_json=json_set(capabilities_json,'$.sprite_wake_failed_at',?) WHERE id=?`, formatHubTime(heartbeat.Add(time.Second)), runner.MachineID); err != nil {
						t.Fatal(err)
					}
				}
				assertSpritePostgresFacts(t, f)
				if typed {
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET local_checks_json='{}' WHERE organization_id=?`, scope.organization); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM runner_local_checks WHERE runner_id IN (SELECT id FROM runner_identities WHERE organization_id=?)`, scope.organization); err != nil {
						t.Fatal(err)
					}
				}
				allowed, err := spriteRunnerAllowedFixture(t, f, scope, runner.RunnerID)
				if err != nil || allowed != test.allowed {
					t.Fatalf("allowed=%t err=%v, want %t", allowed, err, test.allowed)
				}
				hosted := f.service.config.Hosted
				f.service.config.Hosted = nil
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/sprite-pool", testHubAdminToken, nil)
				f.service.config.Hosted = hosted
				requireNativeStatus(t, response, http.StatusOK)
				var view spritePoolView
				decodeHubResponse(t, response, &view)
				if len(view.Members) != 1 || view.Members[0].Status != test.status {
					t.Fatalf("members=%+v, want status %s", view.Members, test.status)
				}
				var commands []string
				for _, step := range view.Members[0].WaitingFor {
					commands = append(commands, step.Command)
				}
				if strings.Join(commands, "\n") != strings.Join(test.commands, "\n") {
					t.Fatalf("waiting for %q, want %q", commands, test.commands)
				}
			})
		}
	}
}

func TestSpriteLifecycleReadsArchivedRunDetails(t *testing.T) {
	f, scope, provider := newSpritePoolFixture(t, "")
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if err := f.service.stopGitHubReconciliation(); err != nil {
		t.Fatal(err)
	}
	f.service.outbox.stop()
	keys, hosted := f.service.config.SecretKeys, f.service.config.Hosted
	f.service.config.SecretKeys, f.service.config.Hosted = nil, nil
	store := &memoryRunDetails{}
	f.service.config.RunDetails = store
	f.service.config.Content = nil
	r := placementFixtureRunner(t, f, "archive-worker", "local", false, 1, "gpt-6.1-sol", "test-model")
	issue := f.create(t, "Archived execution")
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, issue, "archived-run"))
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	event := nativeStartedEvent(lease)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", r.redemption.Credential, event), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET status='succeeded',work_item_revision=1000,dispatch_generation=1000 WHERE id=?`, event.Data.AttemptID); err != nil {
		t.Fatal(err)
	}
	setPlacementFixture(t, f, policy.Placement{Mode: "sprites_only"}, 2)
	f.service.config.SecretKeys, f.service.config.Hosted = keys, hosted
	store.mu.Lock()
	store.reads = 0
	store.mu.Unlock()
	f.service.scheduleSpriteLifecycle(t.Context(), scope, "Todo")
	f.service.spriteWakeWork.Wait()
	store.mu.Lock()
	reads := store.reads
	store.mu.Unlock()
	if reads == 0 || len(provider.created) != 2 {
		t.Fatalf("archived run reads=%d created Sprites=%d", reads, len(provider.created))
	}
}
