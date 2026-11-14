package hubserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/budget"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
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

func TestSpritePoolPlacement(t *testing.T) {
	t.Parallel()
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
	tokens           []string
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

func (p *spritePoolProvider) scriptToken(script string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, token := range p.tokens {
		if strings.Contains(script, token) {
			return token
		}
	}
	return ""
}

func newSpritePoolFixture(t *testing.T, failure string) (nativeFixture, hubscope.NativeScope, *spritePoolProvider) {
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
			_ = json.NewEncoder(w).Encode(body)
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
		token := provider.scriptToken(string(script))
		if token == "" || strings.Contains(r.URL.RawQuery, token) {
			t.Error("fresh enrollment must travel only through stdin")
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
			output := "Customer setup completed\nRunner bootstrap started\nInstalling PostgreSQL client " + token + "\nE: Version '" + spritesSecretSentinel + "' for 'postgresql-client-17' was not found\nAuthorization: Bearer private-provider-secret\nBootstrap stopped. Last completed step: GitHub CLI, ripgrep and fd installed.\n"
			_ = conn.Write(r.Context(), websocket.MessageBinary, append([]byte{1}, []byte(output[:20])...))
			_ = conn.Write(r.Context(), websocket.MessageBinary, append([]byte{1}, []byte(output[20:])...))
			_ = conn.Write(r.Context(), websocket.MessageBinary, append([]byte{1}, []byte(strings.Repeat("E: Failed to fetch https://private-provider-secret\n", 1000))...))
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"exit","exit_code":100}`))
			return
		}
		name := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/sprites/"), "/")[0]
		binding := runnerauth.NewBinding()
		credential, err := apikey.GenerateToken()
		if err != nil {
			t.Error(err)
			return
		}
		registration := strings.ReplaceAll(string(script), `'"'"'`, "'")
		tier := "native-trusted"
		if strings.Contains(registration, "--isolation-tier 'sandbox'") {
			tier = "sandbox"
		}
		if !strings.Contains(registration, "--isolation-tier '"+tier+"'") {
			t.Error("registration must carry pool access tier")
		}
		redemption := runnerauth.Redemption{IsolationTier: tier, Binding: binding, Credential: credential, Hostname: name, SpriteName: name, DisplayName: name, Capacity: 1, Version: "fixture"}
		response := performHubAPIRequest(t, provider.f.service, http.MethodPost, "/api/v2/organizations/"+string(provider.f.project.OrganizationID)+"/runner-enrollments/redeem", token, redemption)
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
	f.service.config.Hosted = &hostedplans.HostedConfig{OrganizationID: string(f.project.OrganizationID), PublicURL: "https://hub.example.test"}
	f.service.config.Version = "v0.117.99"
	f.service.config.deps.GenerateToken = func() (string, error) {
		token, err := apikey.GenerateToken()
		provider.mu.Lock()
		provider.token = token
		provider.tokens = append(provider.tokens, token)
		provider.mu.Unlock()
		return token, err
	}
	provider.f = f
	hosted := f.service.config.Hosted
	f.service.config.Hosted = nil
	setPlacementFixture(t, f, policy.Placement{Mode: "blended"}, 2)
	f.service.config.Hosted = hosted
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
	t.Parallel()
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
		{"billing failure is retained for onboarding", "billing", 1, 1, 0, false, nil, false, "", nil},
		{"token rejection is retained for onboarding", "invalid token", 1, 1, 0, false, nil, false, "", nil},
		{"empty bootstrap with declared setup", "", 2, 2, 2, false, new(true), true, "", nil},
		{"empty bootstrap without setup", "", 2, 2, 2, false, new(false), true, "", nil},
		{"legacy hex names survive reconciliation and retirement", "", 2, 2, 2, false, nil, false, "", []string{"detent-0123456789abcdef0123456789abcdef", "detent-fedcba9876543210fedcba9876543210"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, test.failure)
			if test.tier != "" {
				spriteFixtureExec(t, f, `UPDATE organization_sprite_pools SET isolation_tier=$1 WHERE organization_id=$2`, test.tier, scope.Organization)
			}
			if test.emptyBootstrap {
				spriteFixtureExec(t, f, `UPDATE organization_sprite_pools SET bootstrap_object_key=NULL WHERE organization_id=$1`, scope.Organization)
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
					r, err := readSpriteRunnerFixture(t, f, member.RunnerID)
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
			status, err := f.service.coordinatorSpritePoolStatus(t.Context(), scope, view, "/settings/integrations#sprites", scope.Project)
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
				setSpriteIssuesArchived(t, f, true)
				setSpriteMembersIdleSince(t, f, f.service.config.Clock().Add(-time.Hour))
				if test.heartbeat {
					runner := provider.runners[0]
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(runner.MachineID)+"/heartbeat", runner.Credential, map[string]any{"display_name": runner.DisplayName, "capacity": runner.Capacity, "version": runner.Version, "sprite_name": runner.SpriteName})
					requireNativeStatus(t, response, http.StatusOK)
				} else {
					f.service.startSpriteLifecycle(scope, "")
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
	t.Parallel()
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
				createSpriteMemberFixture(t, f, runnerstore.SpriteMembership{Name: "sprite-bright-badger", ProviderOrganization: "detent-test", EnrollmentID: historical.enrollment.ID, State: "deleted"})
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
					if err := nativeAttemptTestDatabase(t, f).QueryRowContext(t.Context(), `SELECT m.state,e.revoked_at IS NOT NULL FROM runner_sprite_memberships m JOIN runner_enrollments e ON e.organization_id=m.organization_id AND e.id=m.enrollment_id WHERE m.organization_id=$1 AND m.name=$2`, scope.Organization, name).Scan(&state, &revoked); err != nil {
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
	t.Parallel()
	for _, test := range []struct {
		name                            string
		runners                         int
		reports, paused, wake, separate bool
		wantFree, wantPending           int
	}{
		{name: "unauthenticated enrollment is not ready", runners: 1},
		{name: "provider bounds reported local capacity", runners: 1, reports: true, wantFree: 1},
		{name: "shared subscription is counted once", runners: 2, reports: true, wantFree: 1},
		{name: "explicit independent accounts add capacity", runners: 2, reports: true, separate: true, wantFree: 2},
		{name: "paused member contributes no ready capacity", runners: 1, reports: true, paused: true},
		{name: "waking a paused member avoids creating a replacement", runners: 1, reports: true, paused: true, wake: true, wantPending: 1},
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
					report := capacityReport(f.service.config.Clock())
					report.Models = []string{"gpt-6.1-sol"}
					if test.separate {
						report.SharedAccountAlias = string(rune('a' + i))
					}
					placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
						return w.ReplaceRunnerProviderReports(t.Context(), r.binding.RunnerID, []providercapacity.Report{report})
					})
				}
				if test.paused {
					placementFixtureHeartbeat(t, f, r.binding.RunnerID, f.service.config.Clock().Add(-time.Minute))
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
			if input.Free != test.wantFree || input.Pending != test.wantPending {
				t.Fatalf("capacity free=%d pending=%d, want free=%d pending=%d", input.Free, input.Pending, test.wantFree, test.wantPending)
			}
		})
	}
}

func TestSpritePoolFreshAuthority(t *testing.T) {
	t.Parallel()
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
			switch test.change {
			case "secret":
				spriteFixtureExec(t, f, `DELETE FROM organization_secrets WHERE organization_id=$1`, scope.Organization)
			case "actor":
				placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
					pool, err := w.(postgres.SpriteReader).SpritePool(t.Context())
					if err != nil {
						return err
					}
					return w.RevokeAccessCredential(t.Context(), pool.ConfiguredBy, f.service.config.Clock())
				})
			case "disable":
				spriteFixtureExec(t, f, `UPDATE organization_sprite_pools SET max_runners=0 WHERE organization_id=$1`, scope.Organization)
			case "project":
				scope.Organization = "org_foreign"
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
	t.Parallel()
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
	t.Parallel()
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
			setSpriteIssuesArchived(t, f, true)
			setSpriteMembersIdleSince(t, f, f.service.config.Clock().Add(-time.Hour))
			if test.managed && test.change != "busy ceiling" && test.change != "retiring" {
				setSpriteAllocationFixture(t, f, 0, 1)
			}
			switch test.change {
			case "busy", "busy ceiling":
				if test.change == "busy ceiling" {
					spriteFixtureExec(t, f, `UPDATE organization_sprite_pools SET max_runners=0 WHERE organization_id=$1`, scope.Organization)
				}
				now := f.service.config.Clock()
				insertSpriteLeaseFixture(t, f, "lease-busy", "session-busy", "", now, now.Add(time.Hour))
			case "floor":
				if test.managed {
					setSpriteAllocationFixture(t, f, 1, 2)
					break
				}
				spriteFixtureExec(t, f, `UPDATE organization_sprite_pools SET min_runners=1 WHERE organization_id=$1`, scope.Organization)
			case "recent":
				setSpriteMembersIdleSince(t, f, f.service.config.Clock())
			case "expired":
				spriteFixtureExec(t, f, `UPDATE runner_sprite_memberships SET state='bootstrapping' WHERE organization_id=$1`, scope.Organization)
				spriteFixtureExec(t, f, `UPDATE runner_enrollments SET created_at=LEAST(created_at,$1::timestamptz-interval '1 hour'),expires_at=$1 WHERE organization_id=$2 AND id IN (SELECT enrollment_id FROM runner_sprite_memberships WHERE organization_id=$2)`, f.service.config.Clock().Add(-time.Minute), scope.Organization)
			case "organization":
				spriteFixtureExec(t, f, `UPDATE organization_secrets SET organization_slug='another-org' WHERE organization_id=$1`, scope.Organization)
			case "retiring":
				spriteFixtureExec(t, f, `UPDATE organization_sprite_pools SET max_runners=1 WHERE organization_id=$1`, scope.Organization)
				view, err := f.service.readSpritePool(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				spriteFixtureExec(t, f, `UPDATE runner_sprite_memberships SET state='deleting' WHERE organization_id=$1 AND name=$2`, scope.Organization, view.Members[0].Name)
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
			err := f.service.scaleSpritePool(t.Context(), scope, false)
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
	t.Parallel()
	f, scope, provider := newSpritePoolFixture(t, "")
	if err := f.service.scaleSpritePool(t.Context(), scope, true); err != nil {
		t.Fatal(err)
	}
	setSpriteIssuesArchived(t, f, true)
	setSpriteMembersIdleSince(t, f, f.service.config.Clock().Add(-time.Hour))
	provider.deleteStarted = make(chan struct{})
	provider.releaseDelete = make(chan struct{})
	f.service.startSpriteLifecycle(scope, "")
	waitSpriteWake(t, provider.deleteStarted)
	setSpriteIssuesArchived(t, f, false)
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
			f := newNativeFixture(t, newLocalHTTPService(t), "", "Example project")
			path := f.base + "/sprite-pool"
			var before spritePoolView
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil), &before)
			if test.legacy {
				response := performHubAPIRequest(t, f.service, http.MethodPut, path, testHubAdminToken, map[string]any{"min_runners": 0, "max_runners": 0, "revision": before.Revision})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &before)
				spriteFixtureExec(t, f, `UPDATE organization_sprite_pools SET isolation_tier='sandbox' WHERE organization_id=$1`, f.project.OrganizationID)
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

func spriteFixtureExec(t *testing.T, f nativeFixture, query string, args ...any) {
	t.Helper()
	if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func setSpriteIssuesArchived(t *testing.T, f nativeFixture, archived bool) {
	t.Helper()
	spriteFixtureExec(t, f, `UPDATE issues SET archived=$1 WHERE organization_id=$2 AND project_id=$3`, archived, f.project.OrganizationID, f.project.ID)
}

func setSpriteMembersIdleSince(t *testing.T, f nativeFixture, at time.Time) {
	t.Helper()
	spriteFixtureExec(t, f, `UPDATE runner_sprite_memberships SET idle_since=$1 WHERE organization_id=$2`, at, f.project.OrganizationID)
}

func insertSpriteLeaseFixture(t *testing.T, f nativeFixture, lease, session, runner string, acquired, expires time.Time) {
	t.Helper()
	if !expires.After(acquired) {
		acquired = expires.Add(-time.Hour)
	}
	query := `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at)
SELECT i.organization_id,$1::text,i.id,r.machine_id,$2::text,i.revision,$3::timestamptz,$3::timestamptz,$4::timestamptz
FROM issues i JOIN runner_identities r ON r.organization_id=i.organization_id
WHERE i.organization_id=$5 AND i.project_id=$6`
	args := []any{lease, session, acquired, expires, f.project.OrganizationID, f.project.ID}
	if runner != "" {
		query += ` AND r.runner_id=$7`
		args = append(args, runner)
	}
	spriteFixtureExec(t, f, query+` LIMIT 1`, args...)
}

func createSpriteMemberFixture(t *testing.T, f nativeFixture, member runnerstore.SpriteMembership) {
	t.Helper()
	now := f.service.config.Clock()
	member.OrganizationID, member.IdleSince, member.CreatedAt = f.project.OrganizationID, now, now
	placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
		return w.CreateRunnerSpriteMembership(t.Context(), member)
	})
}

func setSpriteAllocationFixture(t *testing.T, f nativeFixture, desired int, revision tracker.Revision) projectSpriteAllocation {
	t.Helper()
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}
	var result projectSpriteAllocation
	placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
		owner := w.(postgres.SpriteWriter)
		if _, err := owner.SpritePool(t.Context()); err != nil {
			return err
		}
		credential, err := w.CredentialByHash(t.Context(), apikey.HashToken(testHubAdminToken))
		if err != nil {
			return err
		}
		if err := w.LockWorkItemProject(t.Context(), scope.Project); err != nil {
			return err
		}
		current, row, err := projectSpriteAllocationRecord(t.Context(), w, scope)
		if err != nil {
			return err
		}
		if current.Revision != revision {
			return hubapi.NativeConflict(current.Revision)
		}
		row.OrganizationID, row.ProjectID, row.DesiredCount, row.ConfiguredBy = scope.Organization, scope.Project, int32(desired), credential.ID
		if row.RequestedAt.IsZero() || desired >= current.AllocatedCount {
			row.RequestedAt = f.service.config.Clock()
		}
		if _, err := owner.PutSpriteAllocation(t.Context(), row, int64(revision)); err != nil {
			return err
		}
		result, _, err = projectSpriteAllocationRecord(t.Context(), w, scope)
		return err
	})
	return result
}

func readSpriteAllocationFixture(t *testing.T, f nativeFixture, scope hubscope.NativeScope) (projectSpriteAllocation, error) {
	t.Helper()
	var result projectSpriteAllocation
	err := f.service.accessRead(t.Context(), scope.Organization, func(r tenantstore.Reader) error {
		var err error
		result, _, err = projectSpriteAllocationRecord(t.Context(), r, scope)
		return err
	})
	return result, err
}

func readSpritePoolFixture(t *testing.T, service *Service, organization tracker.OrganizationID) postgres.SpritePool {
	t.Helper()
	var pool postgres.SpritePool
	if err := service.accessRead(t.Context(), organization, func(r tenantstore.Reader) error {
		var err error
		pool, err = r.(postgres.SpriteReader).SpritePool(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return pool
}

func readSpriteRunnerFixture(t *testing.T, f nativeFixture, id string) (runnerauth.Runner, error) {
	t.Helper()
	var result runnerauth.Runner
	err := f.service.accessRead(t.Context(), f.project.OrganizationID, func(r tenantstore.Reader) error {
		owner, ok := r.(runnerstore.RoutingReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		now := f.service.config.Clock()
		record, err := owner.RunnerRoutingRecord(t.Context(), id, now)
		if err != nil {
			return err
		}
		urgent, err := r.RunnerUrgentUpdate(t.Context())
		if err != nil {
			return err
		}
		result = runnerFromRoutingRecord(record, now, urgent)
		return nil
	})
	return result, err
}

func readSpriteRunnerIdentityFixture(t *testing.T, f nativeFixture, id string) (runnerstore.Identity, error) {
	t.Helper()
	var result runnerstore.Identity
	err := f.service.accessRead(t.Context(), f.project.OrganizationID, func(r tenantstore.Reader) error {
		var err error
		result, err = r.RunnerIdentity(t.Context(), id)
		return err
	})
	return result, err
}

func setSpriteRunnerChecksFixture(t *testing.T, f nativeFixture, runner string, project tracker.ProjectID, checks runnerauth.LocalChecks) {
	t.Helper()
	checks.ObservedAt = f.service.config.Clock()
	placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
		return w.ReplaceRunnerLocalChecks(t.Context(), runner, project, checks)
	})
}

func setSpriteMemberChecks(t *testing.T, f nativeFixture, checks runnerauth.LocalChecks) {
	t.Helper()
	checks.ObservedAt = f.service.config.Clock()
	placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
		members, err := w.(postgres.SpriteReader).SpritePoolMembers(t.Context())
		if err != nil {
			return err
		}
		for _, member := range members {
			if member.AllocationProjectID != string(f.project.ID) || member.RunnerID == "" {
				continue
			}
			if err := w.ReplaceRunnerLocalChecks(t.Context(), member.RunnerID, f.project.ID, checks); err != nil {
				return err
			}
		}
		return nil
	})
}

var spriteMemberReady = runnerauth.LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex"}}

func TestProjectSpriteAllocationCommands(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	setPlacementFixture(t, f, policy.Placement{Mode: "blended"}, 2)
	later := newNativeFixture(t, f.service, f.project.OrganizationID, "later")
	for _, project := range []nativeFixture{f, later} {
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
			response := performHubAPIRequest(t, f.service, http.MethodGet, project.base+"/sprite-pool/allocation", project.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var allocation projectSpriteAllocation
			decodeHubResponse(t, response, &allocation)
			if !allocation.Managed || allocation.Revision != 1 || allocation.DesiredCount != 5 || allocation.Shortfall != 5 || allocation.AllocatedCount != 0 {
				t.Fatalf("allocation = %+v", allocation)
			}
		})
	}
}

func TestSpritePoolOperatorTools(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
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
	t.Parallel()
	for _, order := range []string{"oldest", "project ID tie"} {
		t.Run(order, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "")
			hosted := f.service.config.Hosted
			f.service.config.Hosted = nil
			later := newNativeFixture(t, f.service, f.project.OrganizationID, "later")
			f.service.config.Hosted = hosted
			setSpriteAllocationFixture(t, f, 3, 0)
			setSpriteAllocationFixture(t, later, 3, 0)
			first, second := f, later
			if order == "oldest" {
				spriteFixtureExec(t, f, `UPDATE project_sprite_allocations SET requested_at=$1 WHERE organization_id=$2 AND project_id=$3`, f.service.config.Clock().Add(-time.Hour), scope.Organization, later.project.ID)
				first, second = later, f
			} else {
				spriteFixtureExec(t, f, `UPDATE project_sprite_allocations SET requested_at=$1 WHERE organization_id=$2`, f.service.config.Clock(), scope.Organization)
				if later.project.ID < f.project.ID {
					first, second = later, f
				}
			}
			f.service.startSpriteLifecycle(scope, "")
			f.service.spriteWakeWork.Wait()
			read := func(project nativeFixture) projectSpriteAllocation {
				t.Helper()
				allocation, err := readSpriteAllocationFixture(t, f, hubscope.NativeScope{Organization: scope.Organization, Project: project.project.ID})
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
					allowed, err := f.service.spriteAllocationRunnerAllowed(t.Context(), hubscope.NativeScope{Organization: scope.Organization, Project: project.project.ID}, runner.RunnerID)
					if err != nil || allowed != (project.project.ID == first.project.ID) {
						t.Fatalf("claim project=%s allowed=%t err=%v", project.project.ID, allowed, err)
					}
				}
				identity, err := readSpriteRunnerIdentityFixture(t, f, runner.RunnerID)
				if err != nil || identity.Scope != "projects" {
					t.Fatalf("runner scope=%s err=%v", identity.Scope, err)
				}
			}
			setSpriteAllocationFixture(t, first, 1, 1)
			setSpriteMembersIdleSince(t, f, f.service.config.Clock().Add(-time.Hour))
			if err := f.service.scaleSpritePool(t.Context(), scope, true); err != nil {
				t.Fatal(err)
			}
			if a, b := read(first), read(second); a.AllocatedCount != 1 || b.AllocatedCount != 1 || b.Shortfall != 2 {
				t.Fatalf("after freeing first=%+v second=%+v", a, b)
			}
			if len(provider.created) != 3 || len(provider.deleted) != 1 {
				t.Fatalf("created=%d deleted=%d", len(provider.created), len(provider.deleted))
			}
			spriteFixtureExec(t, f, `UPDATE organization_sprite_pools SET max_runners=1 WHERE organization_id=$1`, scope.Organization)
			setSpriteMembersIdleSince(t, f, f.service.config.Clock().Add(-time.Hour))
			f.service.startSpriteLifecycle(scope, "")
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
	t.Parallel()
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
			setSpriteAllocationFixture(t, f, 2, 0)
			setMonthlyBudgetFixture(t, f, false, budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, SpriteMicros: new(int64)})
			if managed {
				setSpriteAllocationFixture(t, later, 2, 0)
			} else {
				approveHubTestPolicy(t, f.service, later.base+"/policy", hubTestPolicy())
				later.create(t, "later first")
				later.create(t, "later second")
			}
			spriteFixtureExec(t, f, `UPDATE project_sprite_allocations SET requested_at=$1 WHERE organization_id=$2 AND project_id=$3`, f.service.config.Clock().Add(-time.Hour), scope.Organization, f.project.ID)
			f.service.config.Hosted, f.service.config.SecretKeys = hosted, keys
			if err := f.service.scaleSpritePool(t.Context(), scope, true); err != nil {
				t.Fatal(err)
			}
			oldest, err := readSpriteAllocationFixture(t, f, scope)
			if err != nil || oldest.AllocatedCount != 0 || oldest.Shortfall != 2 {
				t.Fatalf("budget-blocked allocation=%+v err=%v", oldest, err)
			}
			view, err := f.service.readSpritePoolController(t.Context(), scope)
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
	spriteFixtureExec(t, f, `UPDATE organization_sprite_pools SET max_runners=1 WHERE organization_id=$1`, scope.Organization)
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
		allowed, err := f.service.spriteAllocationRunnerAllowed(t.Context(), scope, runner.RunnerID)
		if err != nil || allowed {
			t.Fatalf("desired zero allowed=%t err=%v", allowed, err)
		}
	}
}

func TestProjectSpriteAllocationLimits(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                              string
		organizationMembers, providerLimit, wantAllocated int
	}{
		{"organization allocations retain the shared ceiling", 2, 0, 1},
		{"reported provider limit includes every reservation", 0, 2, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "")
			spriteFixtureExec(t, f, `UPDATE organization_sprite_pools SET min_runners=$1,max_runners=3 WHERE organization_id=$2`, test.organizationMembers, scope.Organization)
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
			allocation, err := readSpriteAllocationFixture(t, f, scope)
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
				allowed, err := f.service.spriteAllocationRunnerAllowed(t.Context(), scope, manual.binding.RunnerID)
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
	t.Parallel()
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
		t.Run(test.name, func(t *testing.T) {
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
				spriteFixtureExec(t, f.nativeFixture, `UPDATE machines SET sprite_name=hostname WHERE organization_id=$1 AND id=$2`, f.project.OrganizationID, f.runner.binding.MachineID)
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
				createSpriteMemberFixture(t, f.nativeFixture, runnerstore.SpriteMembership{AllocationProjectID: allocationProject, Name: f.runner.redemption.Hostname, ProviderOrganization: "test", EnrollmentID: f.runner.enrollment.ID, State: state})
				if !test.unready {
					setSpriteMemberChecks(t, owner, spriteMemberReady)
				}
			}
			session := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
			item := f.item(t, session.ID)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.runner.redemption.Credential, tracker.NativeClaim{PolicyID: f.policy, WorkItemID: tracker.NativeWorkItemID(item), MachineID: f.runner.binding.MachineID, SessionID: hubapi.NewID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}})
			requireNativeStatus(t, response, test.status)
		})
	}
}

func TestHostedProjectSpriteAllocationAuthority(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false)
	const organization = tracker.OrganizationID("org_browser_preview")
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), "user_browser_owner")
		if err != nil {
			return err
		}
		_, err = w.(postgres.SpriteWriter).PutSpritePool(t.Context(), postgres.SpritePool{OrganizationID: organization, MaxRunners: 2, IdleSeconds: 300, ConfiguredBy: member.PrincipalID, IsolationTier: "native-trusted", PlacementMode: "blended"}, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cookie := f.cookies["owner"]
	if cookie == nil {
		t.Fatal("missing owner session")
	}
	response := f.rawAPI(t, "owner", http.MethodPut, "/api/v2/organizations/org_browser_preview/projects/"+f.project+"/sprite-pool/allocation", `{"idempotency_key":"allocate","expected_revision":"0","desired_count":2}`, map[string]string{"X-CSRF-Token": hostedCSRF(cookie.Value)})
	browserHostedStatus(t, response, http.StatusOK)
	scope := hubscope.NativeScope{Organization: organization, Project: tracker.ProjectID(f.project)}
	authority := func() (bool, error) {
		var allowed bool
		err := f.service.accessRead(t.Context(), organization, func(r tenantstore.Reader) error {
			var err error
			allowed, err = f.service.projectSpriteAllocationAuthority(t.Context(), r, scope)
			return err
		})
		return allowed, err
	}
	allowed, err := authority()
	if err != nil || !allowed {
		t.Fatalf("configuring owner allowed=%t err=%v", allowed, err)
	}
	if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), `UPDATE hosted_project_grants SET can_write=false WHERE organization_id=$1 AND project_id=$2 AND user_id='user_browser_owner'`, organization, f.project); err != nil {
		t.Fatal(err)
	}
	allowed, err = authority()
	if err != nil || allowed {
		t.Fatalf("revoked project grant allowed=%t err=%v", allowed, err)
	}
}

func projectSpriteMembers(t *testing.T, f nativeFixture, scope hubscope.NativeScope) []spritePoolMember {
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
	t.Parallel()
	f, scope, provider := newSpritePoolFixture(t, "")
	spriteFixtureExec(t, f, `UPDATE organization_sprite_pools SET max_runners=3 WHERE organization_id=$1`, scope.Organization)
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
	t.Parallel()
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
			allowed, err := f.service.spriteAllocationRunnerAllowed(t.Context(), scope, provider.runners[0].RunnerID)
			if err != nil || allowed {
				t.Fatalf("late member allowed=%t err=%v", allowed, err)
			}
			allocation, err := readSpriteAllocationFixture(t, f, scope)
			if err != nil || allocation.ProvisionFailures != 0 {
				t.Fatalf("stale reservation counted as failure: %+v err=%v", allocation, err)
			}
		})
	}
}

func TestProjectSpriteDrain(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		expired bool
		retire  func(t *testing.T, f nativeFixture, runner string)
	}{
		{name: "active work publishes before deletion", retire: func(t *testing.T, f nativeFixture, _ string) {
			spriteFixtureExec(t, f, `UPDATE leases SET released_at=$1 WHERE organization_id=$2 AND lease_id='lease-drain'`, f.service.config.Clock(), f.project.OrganizationID)
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
			now := f.service.config.Clock()
			expires := now.Add(time.Hour)
			if test.expired {
				expires = now.Add(-time.Hour)
				placementFixtureHeartbeat(t, f, member.RunnerID, now.Add(-time.Hour))
			}
			insertSpriteLeaseFixture(t, f, "lease-drain", "session-drain", member.RunnerID, now, expires)
			placementFixtureWrite(t, f, func(w tenantstore.Writer) error {
				return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: scope.Organization, LeaseID: "lease-drain", RunnerID: member.RunnerID, AssignedAt: now})
			})
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
			identity, err := readSpriteRunnerIdentityFixture(t, f, member.RunnerID)
			if err != nil || identity.State != "draining" {
				t.Fatalf("runner state=%q err=%v", identity.State, err)
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
	t.Parallel()
	for _, test := range []struct {
		name    string
		rewrite bool
	}{
		{"failed provisioning keeps making progress", false},
		{"a rewrite during an attempt preserves current failures", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "bootstrap")
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
				rewritten, err := readSpriteAllocationFixture(t, f, scope)
				if err != nil || rewritten.Revision != 2 || rewritten.ProvisionFailures != 0 {
					t.Fatalf("old attempt charged the rewritten request: %+v err=%v", rewritten, err)
				}
			}
			const failedAttempts = 5
			for range failedAttempts {
				_ = f.service.scaleSpritePool(t.Context(), scope, false)
			}
			provider.fail = ""
			if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
				t.Fatal(err)
			}
			held, err := readSpriteAllocationFixture(t, f, scope)
			if err != nil || held.ProvisionFailures != failedAttempts || held.Shortfall != 0 || len(provider.created) != failedAttempts+1+map[bool]int{true: 1}[test.rewrite] || len(provider.deleted) != failedAttempts+map[bool]int{true: 1}[test.rewrite] {
				t.Fatalf("allocation=%+v created=%d deleted=%d err=%v", held, len(provider.created), len(provider.deleted), err)
			}
			setSpriteAllocationFixture(t, f, 1, held.Revision)
			if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
				t.Fatal(err)
			}
			if members := projectSpriteMembers(t, f, scope); len(members) != 1 || members[0].State != "enrolled" || len(provider.created) != failedAttempts+1+map[bool]int{true: 1}[test.rewrite] {
				t.Fatalf("successful allocation survives rewrite members=%+v created=%d", members, len(provider.created))
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
		spriteFixtureExec(t, f, `UPDATE runner_sprite_memberships SET state='bootstrapping' WHERE organization_id=$1 AND name=$2`, scope.Organization, interrupted.Name)
		spriteFixtureExec(t, f, `UPDATE runner_enrollments SET created_at=LEAST(created_at,$1::timestamptz-interval '1 hour'),expires_at=$1 WHERE organization_id=$2 AND id=$3`, f.service.config.Clock().Add(-time.Minute), scope.Organization, interrupted.EnrollmentID)
		if err := f.service.scaleSpritePool(t.Context(), scope, false); err != nil {
			t.Fatal(err)
		}
		members := projectSpriteMembers(t, f, scope)
		if len(members) != 1 || members[0].Name == interrupted.Name || members[0].State != "enrolled" || len(provider.created) != 2 || len(provider.deleted) != 1 || provider.deleted[0] != interrupted.Name {
			t.Fatalf("members=%+v created=%v deleted=%v", members, provider.created, provider.deleted)
		}
		allocation, err := readSpriteAllocationFixture(t, f, scope)
		if err != nil || allocation.ProvisionFailures != 1 {
			t.Fatalf("allocation=%+v err=%v", allocation, err)
		}
	})
}

func TestProjectSpriteLoginReadiness(t *testing.T) {
	t.Parallel()
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
		t.Run(test.name, func(t *testing.T) {
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
			heartbeat := f.service.config.Clock().Add(-test.heartbeatAgo)
			spriteFixtureExec(t, f, `UPDATE runner_identities SET last_heartbeat_at=$1 WHERE organization_id=$2 AND machine_id=$3`, heartbeat, scope.Organization, runner.MachineID)
			if test.wakeFailed {
				spriteFixtureExec(t, f, `UPDATE machines SET sprite_wake_failed_at=$1 WHERE organization_id=$2 AND id=$3`, heartbeat.Add(time.Second), scope.Organization, runner.MachineID)
			}
			allowed, err := f.service.spriteAllocationRunnerAllowed(t.Context(), scope, runner.RunnerID)
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

type countedRunDetails struct {
	attachment.DetailStore
	mu    sync.Mutex
	reads int
}

func (c *countedRunDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.DetailStore.ReadDetail(ctx, organization, ref)
}

func TestSpriteLifecycleReadsArchivedRunDetails(t *testing.T) {
	t.Parallel()
	f, scope, provider := newSpritePoolFixture(t, "")
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	f.service.outbox.stop()
	keys, hosted := f.service.config.SecretKeys, f.service.config.Hosted
	f.service.config.SecretKeys, f.service.config.Hosted = nil, nil
	store := &countedRunDetails{DetailStore: f.service.config.RunDetails}
	f.service.config.RunDetails = store
	r := placementFixtureRunner(t, f, "archive-worker", "local", false, 1, "gpt-6.1-sol", "test-model")
	issue := f.create(t, "Archived execution")
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, issue, "archived-run"))
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	event := nativeStartedEvent(lease)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", r.redemption.Credential, event), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
	spriteFixtureExec(t, f, `UPDATE native_attempts SET status='succeeded' WHERE organization_id=$1 AND id=$2`, scope.Organization, event.Data.AttemptID)
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

func TestZZSpriteBootstrapProbe(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	f.service.config.Hosted = &hostedplans.HostedConfig{OrganizationID: string(f.project.OrganizationID), PublicURL: "https://hub.example.test"}
	scope := placementFixtureScope(t, f)
	scope.Project = ""
	if err := f.service.updateSpritePool(t.Context(), scope, spritePoolSettings{MaxRunners: 1, Bootstrap: "true"}, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	view, err := f.service.readSpritePool(t.Context(), scope)
	t.Logf("read after production write: bootstrap=%q revision=%d err=%v", view.Bootstrap, view.Revision, err)
	err = f.service.updateSpritePool(t.Context(), scope, spritePoolSettings{MaxRunners: 1, Bootstrap: "false", Revision: 1}, nil)
	t.Logf("replace bootstrap err=%v", err)
	_, err = f.service.readSpritePoolController(t.Context(), scope)
	t.Logf("controller read err=%v", err)
}
