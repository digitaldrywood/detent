package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestWakeSpriteRunners(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                   string
		heartbeatAgo           time.Duration
		dispatchable           bool
		storeToken             bool
		spriteStatus           int
		wantWoken              int
		wantWakeCalls          int
		organizationToken      bool
		organizationScope      bool
		wrongOrganizationToken bool
	}{
		{name: "paused runner with dispatchable work", heartbeatAgo: time.Minute, dispatchable: true, storeToken: true, spriteStatus: http.StatusOK, wantWoken: 1, wantWakeCalls: 1},
		{name: "organization runner inherits token", heartbeatAgo: time.Minute, dispatchable: true, organizationToken: true, organizationScope: true, spriteStatus: http.StatusOK, wantWoken: 1, wantWakeCalls: 1},
		{name: "project token overrides organization token", heartbeatAgo: time.Minute, dispatchable: true, storeToken: true, organizationToken: true, wrongOrganizationToken: true, spriteStatus: http.StatusOK, wantWoken: 1, wantWakeCalls: 1},
		{name: "organization token cannot cross tenant", heartbeatAgo: time.Minute, dispatchable: true, organizationToken: true, organizationScope: true, wrongOrganizationToken: true, spriteStatus: http.StatusOK},
		{name: "awake runner is left alone", heartbeatAgo: time.Second, dispatchable: true, storeToken: true, spriteStatus: http.StatusOK},
		{name: "terminal or non-dispatchable state", heartbeatAgo: time.Minute, storeToken: true, spriteStatus: http.StatusOK},
		{name: "project without a Sprites token", heartbeatAgo: time.Minute, dispatchable: true, spriteStatus: http.StatusOK},
		{name: "host is not a Sprite in this organization", heartbeatAgo: time.Minute, dispatchable: true, storeToken: true, spriteStatus: http.StatusNotFound, wantWakeCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var calls []*http.Request
			client := &http.Client{Transport: spritesTestTransport(func(request *http.Request) (*http.Response, error) {
				mu.Lock()
				calls = append(calls, request.Clone(request.Context()))
				mu.Unlock()
				return spritesTestResponse(`{"type":"complete"}`, tt.spriteStatus), nil
			})}
			f, scope, runners := newSpriteWakeFixture(t, client, tt.storeToken, tt.heartbeatAgo, "customer-host")
			if tt.organizationScope {
				_, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET scope='organization' WHERE id=?", runners[0].binding.RunnerID)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.service.database.db.ExecContext(t.Context(), "DELETE FROM token_grants WHERE token_id=?", runners[0].binding.RunnerID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if tt.organizationToken {
				token := spritesSecretSentinel
				if tt.wrongOrganizationToken && tt.storeToken {
					token = "detent-test/other/token/value"
				}
				storeOrganizationSpritesSecret(t, f, token)
				if tt.wrongOrganizationToken && !tt.storeToken {
					_, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM organization_secrets WHERE organization_id=?", f.project.OrganizationID)
					if err != nil {
						t.Fatal(err)
					}
					response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations", testHubAdminToken, map[string]any{"name": "Other Sprite organization"})
					requireNativeStatus(t, response, http.StatusCreated)
					var organization nativeOrganization
					decodeHubResponse(t, response, &organization)
					foreign := newNativeFixture(t, f.service, organization.ID, "other")
					storeOrganizationSpritesSecret(t, foreign, token)
				}
			}
			state := ""
			for _, candidate := range f.project.States {
				if (candidate.Dispatchable && !candidate.Terminal) == tt.dispatchable {
					state = candidate.Name
					break
				}
			}
			if state == "" {
				t.Fatalf("fixture project has no state with dispatchable=%t", tt.dispatchable)
			}
			woken, err := f.service.wakeSpriteRunners(t.Context(), scope, state)
			if err != nil {
				t.Fatal(err)
			}
			if woken != tt.wantWoken {
				t.Fatalf("woken = %d, want %d", woken, tt.wantWoken)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(calls) != tt.wantWakeCalls {
				t.Fatalf("wake calls = %d, want %d", len(calls), tt.wantWakeCalls)
			}
			for _, call := range calls {
				if call.Method != http.MethodPost || call.URL.String() != "https://api.sprites.dev/v1/sprites/customer-host/services/detent-runner/start?duration=30s" {
					t.Fatalf("wake request = %s %s", call.Method, call.URL)
				}
				if call.Header.Get("Authorization") != "Bearer "+spritesSecretSentinel {
					t.Fatal("wake request did not carry the project's Sprites token")
				}
			}
		})
	}
}

func newSpriteWakeFixture(t *testing.T, client *http.Client, storeToken bool, heartbeatAgo time.Duration, names ...string) (nativeFixture, nativeScope, []runnerFixture) {
	t.Helper()
	f := newDefaultNativeFixture(t, Config{SecretKeys: secretTestKeys(t, "1", "1"), SpritesHTTPClient: client})
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: "test"}}
	runners := enrollSpriteWakeRunners(t, f, heartbeatAgo, names...)
	if storeToken {
		storeSpriteWakeSecret(t, f, spritesSecretSentinel)
	}
	return f, scope, runners
}

func enrollSpriteWakeRunners(t *testing.T, f nativeFixture, heartbeatAgo time.Duration, names ...string) []runnerFixture {
	t.Helper()
	var runners []runnerFixture
	for _, name := range names {
		runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
		runner.redemption.Hostname = name
		runner.redemption.SpriteName = name
		runner.enroll(t)
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?", formatHubTime(f.service.config.now().Add(-heartbeatAgo)), runner.binding.RunnerID); err != nil {
			t.Fatal(err)
		}
		runners = append(runners, runner)
	}
	return runners
}

func storeSpriteWakeSecret(t *testing.T, f nativeFixture, token string) {
	t.Helper()
	envelope, err := f.service.config.SecretKeys.Seal([]byte(token), secretAAD(string(f.project.OrganizationID), string(f.project.ID), flySpritesToken))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO project_secrets(organization_id, project_id, kind, organization_slug, ciphertext, nonce, wrapped_data_key, master_key_version, updated_at) VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(organization_id, project_id, kind) DO UPDATE SET ciphertext=excluded.ciphertext, nonce=excluded.nonce, wrapped_data_key=excluded.wrapped_data_key, master_key_version=excluded.master_key_version`, f.project.OrganizationID, f.project.ID, flySpritesToken, "detent-test", envelope.Ciphertext, envelope.Nonce, envelope.WrappedKey, envelope.Version, formatHubTime(f.service.config.now())); err != nil {
		t.Fatal(err)
	}
}

func waitSpriteWake(t *testing.T, done <-chan struct{}) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatal("Sprite wake work did not reach the fixture checkpoint")
	}
}

func spriteWakeDone(t *testing.T, s *Service, scope nativeScope) <-chan struct{} {
	t.Helper()
	s.spriteWakeMu.Lock()
	defer s.spriteWakeMu.Unlock()
	done := s.spriteWakes[spriteWakeKey{organization: scope.organization}]
	if done == nil {
		t.Fatal("no active Sprite wake pass")
	}
	return done.done
}

func TestWakeSpriteRunnersAfterBurst(t *testing.T) {
	for _, test := range []struct {
		name           string
		status         int
		transportError bool
	}{
		{name: "successful provider", status: http.StatusOK},
		{name: "unavailable provider", status: http.StatusServiceUnavailable},
		{name: "transport error", transportError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{}, 2)
			release := make(chan struct{})
			var calls atomic.Int32
			client := &http.Client{Transport: spritesTestTransport(func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				started <- struct{}{}
				select {
				case <-release:
				case <-request.Context().Done():
					return nil, request.Context().Err()
				}
				if test.transportError {
					return nil, errors.New("fixture provider failure")
				}
				return spritesTestResponse(`{"type":"complete"}`, test.status), nil
			})}
			f, scope, _ := newSpriteWakeFixture(t, client, true, time.Minute, "customer-host")
			f.create(t, "initial")
			waitSpriteWake(t, started)
			done := spriteWakeDone(t, f.service, scope)
			const mutations = 64
			var workers sync.WaitGroup
			latencies := make(chan time.Duration, mutations)
			for i := range mutations {
				workers.Go(func() {
					start := time.Now()
					f.create(t, fmt.Sprintf("burst-%d", i))
					latencies <- time.Since(start)
				})
			}
			mutationsDone := make(chan struct{})
			go func() { workers.Wait(); close(mutationsDone) }()
			waitSpriteWake(t, mutationsDone)
			close(latencies)
			var maximum time.Duration
			for latency := range latencies {
				maximum = max(maximum, latency)
			}
			var profile bytes.Buffer
			if err := pprof.Lookup("goroutine").WriteTo(&profile, 2); err != nil {
				t.Fatal(err)
			}
			retained := strings.Count(profile.String(), "hubserver.(*Service).scheduleSpriteLifecycle.func")
			f.service.spriteWakeMu.Lock()
			active := len(f.service.spriteWakes)
			f.service.spriteWakeMu.Unlock()
			if active != 1 || retained != 1 || calls.Load() != 1 {
				t.Fatalf("burst: active passes=%d, retained wake goroutines=%d, provider calls=%d", active, retained, calls.Load())
			}
			t.Logf("fixture mutations=%d provider_calls=%d retained_wake_goroutines=%d active_passes=%d max_mutation_latency=%s", mutations+1, calls.Load(), retained, active, maximum)
			close(release)
			waitSpriteWake(t, done)
			f.service.spriteWakeWork.Wait()
			f.service.spriteWakeMu.Lock()
			active = len(f.service.spriteWakes)
			f.service.spriteWakeMu.Unlock()
			wantCalls := int32(1)
			if test.status != http.StatusOK || test.transportError {
				wantCalls = 2
			}
			if active != 0 || calls.Load() != wantCalls {
				t.Fatalf("completed burst: active passes=%d provider calls=%d", active, calls.Load())
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE machines SET capabilities_json=json_remove(capabilities_json,'$.sprite_woken_at') WHERE organization_id=?`, scope.organization); err != nil {
				t.Fatal(err)
			}
			f.create(t, "later-mutation")
			waitSpriteWake(t, started)
			f.service.spriteWakeWork.Wait()
			if calls.Load() != wantCalls+1 {
				t.Fatalf("later mutation made %d total calls, want %d", calls.Load(), wantCalls+1)
			}
		})
	}
}

func TestWakeSpriteRunnersFreshAuthority(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change string
		want   int
	}{
		{name: "multiple runners and duplicate hostnames", want: 3},
		{name: "removed project grant", change: "grant", want: 2},
		{name: "revoked runner token", change: "token", want: 2},
		{name: "inactive runner", change: "runner", want: 2},
		{name: "fresh heartbeat", change: "heartbeat", want: 2},
		{name: "removed secret", change: "secret", want: 1},
		{name: "replaced secret", change: "rotation", want: 3},
		{name: "workflow no longer dispatchable", change: "workflow", want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			started, release := make(chan struct{}), make(chan struct{})
			var requests []*http.Request
			client := &http.Client{Transport: spritesTestTransport(func(request *http.Request) (*http.Response, error) {
				requests = append(requests, request.Clone(request.Context()))
				if len(requests) == 1 {
					close(started)
					select {
					case <-release:
					case <-request.Context().Done():
						return nil, request.Context().Err()
					}
				}
				return spritesTestResponse("", http.StatusOK), nil
			})}
			f, scope, runners := newSpriteWakeFixture(t, client, true, time.Minute, "a-runner", "b-runner", "c-runner", "c-runner")
			type outcome struct {
				woken int
				err   error
			}
			result := make(chan outcome, 1)
			go func() {
				woken, err := f.service.wakeSpriteRunners(t.Context(), scope, "Todo")
				result <- outcome{woken, err}
			}()
			waitSpriteWake(t, started)
			id := runners[1].binding.RunnerID
			var query string
			var args []any
			switch test.change {
			case "grant":
				query, args = "DELETE FROM token_grants WHERE token_id=(SELECT token_id FROM runner_identities WHERE id=?)", []any{id}
			case "token":
				query, args = "UPDATE api_tokens SET revoked_at=? WHERE id=(SELECT token_id FROM runner_identities WHERE id=?)", []any{formatHubTime(f.service.config.now()), id}
			case "runner":
				query, args = "UPDATE runner_identities SET state='disabled' WHERE id=?", []any{id}
			case "heartbeat":
				query, args = "UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?", []any{formatHubTime(f.service.config.now()), id}
			case "secret":
				query, args = "DELETE FROM project_secrets WHERE project_id=?", []any{f.project.ID}
			case "rotation":
				storeSpriteWakeSecret(t, f, spritesSecretSentinel+"-rotated")
			case "workflow":
				for i := range f.project.States {
					f.project.States[i].Dispatchable = false
				}
				states, err := json.Marshal(f.project.States)
				if err != nil {
					t.Fatal(err)
				}
				query, args = "UPDATE projects SET states_json=? WHERE id=?", []any{string(states), f.project.ID}
			}
			if query != "" {
				if _, err := f.service.database.db.ExecContext(t.Context(), query, args...); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			select {
			case got := <-result:
				if got.err != nil || got.woken != test.want || len(requests) != test.want {
					t.Fatalf("woken=%d requests=%d error=%v; want %d", got.woken, len(requests), got.err, test.want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("fresh authority wake did not complete")
			}
			for i, request := range requests {
				wantToken := spritesSecretSentinel
				if i > 0 && test.change == "rotation" {
					wantToken += "-rotated"
				}
				if request.Header.Get("Authorization") != "Bearer "+wantToken {
					t.Fatal("wake did not use the current project secret")
				}
			}
			var audits int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM project_secret_audit WHERE project_id=? AND event='use'", f.project.ID).Scan(&audits); err != nil || audits != test.want {
				t.Fatalf("secret use audits=%d error=%v, want %d", audits, err, test.want)
			}
		})
	}
}

func TestWakeSpriteRunnersCancellation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"context", "shutdown", "close"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			started, stopped := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			client := &http.Client{Transport: spritesTestTransport(func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				close(started)
				<-request.Context().Done()
				close(stopped)
				return nil, request.Context().Err()
			})}
			f, scope, _ := newSpriteWakeFixture(t, client, true, time.Minute, "a-runner", "b-runner")
			if mode == "context" {
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() {
					defer close(done)
					_, err := f.service.wakeSpriteRunners(ctx, scope, "Todo")
					if !errors.Is(err, context.Canceled) {
						t.Errorf("wake cancellation error=%v", err)
					}
				}()
				waitSpriteWake(t, started)
				cancel()
				waitSpriteWake(t, done)
			} else {
				f.create(t, "wake-before-stop")
				waitSpriteWake(t, started)
				done := spriteWakeDone(t, f.service, scope)
				if mode == "shutdown" {
					if err := f.service.Shutdown(t.Context()); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := f.service.Close(); err != nil {
						t.Fatal(err)
					}
				}
				waitSpriteWake(t, done)
				f.service.wakeSpriteRunnersAfter(scope, json.RawMessage(`{"state":"Todo"}`))
				f.service.spriteWakeMu.Lock()
				active := len(f.service.spriteWakes)
				f.service.spriteWakeMu.Unlock()
				if active != 0 {
					t.Fatalf("stop retained %d active passes", active)
				}
			}
			waitSpriteWake(t, stopped)
			if calls.Load() != 1 {
				t.Fatalf("cancellation calls=%d, want 1", calls.Load())
			}
		})
	}
}

func TestWakeSpriteRunnersProjectIsolation(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var mu sync.Mutex
	var requests []*http.Request
	client := &http.Client{Transport: spritesTestTransport(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		requests = append(requests, request.Clone(request.Context()))
		mu.Unlock()
		started <- struct{}{}
		select {
		case <-release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		return spritesTestResponse("", http.StatusOK), nil
	})}
	first, firstScope, _ := newSpriteWakeFixture(t, client, true, time.Minute, "first-runner")
	second := newNativeFixture(t, first.service, first.project.OrganizationID, "second")
	secondScope := nativeScope{organization: second.project.OrganizationID, project: second.project.ID, credential: apiCredential{ID: "test"}}
	enrollSpriteWakeRunners(t, second, time.Minute, "second-runner")
	storeSpriteWakeSecret(t, second, spritesSecretSentinel+"-second")
	plain := newNativeFixture(t, first.service, first.project.OrganizationID, "non-sprite")
	enrollSpriteWakeRunners(t, plain, time.Minute, "plain-runner")
	first.create(t, "first-wake")
	second.create(t, "second-wake")
	waitSpriteWake(t, started)
	firstDone := spriteWakeDone(t, first.service, firstScope)
	secondDone := spriteWakeDone(t, first.service, secondScope)
	if firstDone != secondDone {
		t.Fatal("projects in one organization must share a lifecycle pass")
	}
	for i := range 8 {
		first.create(t, fmt.Sprintf("first-%d", i))
		second.create(t, fmt.Sprintf("second-%d", i))
		plain.create(t, fmt.Sprintf("plain-%d", i))
	}
	close(release)
	waitSpriteWake(t, firstDone)
	waitSpriteWake(t, started)
	waitSpriteWake(t, secondDone)
	first.service.spriteWakeWork.Wait()
	if len(requests) != 2 {
		t.Fatalf("isolated project calls=%d, want 2", len(requests))
	}
	for _, request := range requests {
		wantToken := spritesSecretSentinel
		switch request.URL.Path {
		case "/v1/sprites/first-runner/services/detent-runner/start":
		case "/v1/sprites/second-runner/services/detent-runner/start":
			wantToken += "-second"
		default:
			t.Fatalf("wake escaped project grants: %s", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+wantToken {
			t.Fatal("wake used another project's secret")
		}
	}
}

func storeOrganizationSpritesSecret(t *testing.T, f nativeFixture, token string) {
	t.Helper()
	envelope, err := f.service.config.SecretKeys.Seal([]byte(token), secretAAD(string(f.project.OrganizationID), "", flySpritesToken))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO organization_secrets(organization_id,kind,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(organization_id,kind) DO UPDATE SET organization_slug=excluded.organization_slug,ciphertext=excluded.ciphertext,nonce=excluded.nonce,wrapped_data_key=excluded.wrapped_data_key,master_key_version=excluded.master_key_version`, f.project.OrganizationID, flySpritesToken, "detent-test", envelope.Ciphertext, envelope.Nonce, envelope.WrappedKey, envelope.Version, formatHubTime(f.service.config.now()))
	if err != nil {
		t.Fatal(err)
	}
}
