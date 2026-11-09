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

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5"
)

func TestSpriteWakeLostResponseReconcilesAfterServiceReopen(t *testing.T) {
	var mu sync.Mutex
	starts, observations := 0, 0
	client := &http.Client{Transport: spritesTestTransport(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if request.Method == http.MethodPost {
			starts++
			return nil, errors.New("service start committed before response was lost")
		}
		observations++
		return spritesTestResponse(`{"name":"detent-runner","state":{"status":"running"}}`, http.StatusOK), nil
	})}
	f, scope, runners := newSpriteWakeFixture(t, client, true, time.Minute, "first-host", "second-host")
	now := f.service.config.now()
	f.service.config.now = func() time.Time { return now }
	woken, err := f.service.wakeSpriteRunners(t.Context(), scope, "Todo")
	if err != nil || woken != 0 {
		t.Fatalf("uncertain wake count=%d error=%v", woken, err)
	}
	if starts != 2 {
		t.Fatalf("initial service starts=%d", starts)
	}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		for _, runner := range runners {
			operation, err := owner.SpriteEnrollmentWake(t.Context(), runner.enrollment.ID)
			if err != nil {
				return err
			}
			if operation.Outcome != "uncertain" || operation.RequestObjectKey == nil || operation.Attempts != 1 {
				t.Fatalf("wake uncertainty was not durable: %+v", operation)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := Config{DatabaseBackend: DatabasePostgres, Postgres: f.service.config.Postgres, Hosted: &HostedConfig{OrganizationID: string(scope.organization), WorkOSOrganizationID: "provider_startup", PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider()}, SecretKeys: f.service.config.SecretKeys, RunDetails: f.service.config.RunDetails, SpritesHTTPClient: client, now: func() time.Time { return now }}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	resumed := openTestService(t, cfg)
	resumed.maintainSpritePools(t.Context())
	resumed.spriteWakeWork.Wait()
	if err := resumed.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		for _, runner := range runners {
			operation, err := owner.SpriteEnrollmentWake(t.Context(), runner.enrollment.ID)
			if err != nil {
				return err
			}
			if operation.Outcome != "committed" || operation.ReceiptObjectKey == nil || operation.TargetEnrollmentID != runner.enrollment.ID {
				t.Fatalf("wake did not reconcile after restart: %+v", operation)
			}
			if _, err := r.RunnerSpriteMembershipByEnrollment(t.Context(), runner.enrollment.ID); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("manual Sprite acquired managed pool deletion authority: %v", err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if starts != 2 || observations != 2 {
		t.Fatalf("recovery starts=%d observations=%d; want two starts and two observations", starts, observations)
	}
}

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
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), runners[0].binding.RunnerID)
					if err != nil {
						return err
					}
					identity.Scope = "organization"
					if err := w.UpdateRunnerRouting(t.Context(), identity.Revision, identity); err != nil {
						return err
					}
					grants, err := w.AccessTokenGrants(t.Context(), identity.TokenID, "", 200)
					if err != nil {
						return err
					}
					for _, grant := range grants {
						if err := w.DeleteAccessTokenGrant(t.Context(), identity.TokenID, grant.ProjectID); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
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
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.DeleteAccessSecret(t.Context(), "", flySpritesToken, accessstore.SecretAudit{OrganizationID: scope.organization, Actor: "fixture", Kind: flySpritesToken, Event: "delete", RecordedAt: f.service.config.now()})
					}); err != nil {
						t.Fatal(err)
					}
					organization := tracker.OrganizationID("org_foreign")
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Other Sprite organization", Slug: "other-sprites", CreatedAt: f.service.config.now()})
					}); err != nil {
						t.Fatal(err)
					}
					foreign := nativeFixture{service: f.service, project: tracker.NativeProject{OrganizationID: organization}}
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
			assertSpritePostgresFacts(t, f)
			beforeWake := f.service.config.now()
			woken, err := f.service.wakeSpriteRunners(t.Context(), scope, state)
			if err != nil {
				t.Fatal(err)
			}

			afterWake := f.service.config.now()
			wakeTimeInRange := func(value *string) bool {
				at, err := parseTimeValue(spriteFactValue(value))
				return err == nil && !at.Before(beforeWake) && !at.After(afterWake)
			}
			if tt.wantWakeCalls > 0 {
				facts, err := f.service.spriteWakeFactsRecord(t.Context(), scope.organization, runners[0].binding.RunnerID)
				if err != nil || spriteFactValue(facts.Name) != "customer-host" {
					t.Fatalf("wake lost Sprite identity: %+v, %v", facts, err)
				}
				if tt.wantWoken > 0 && (!wakeTimeInRange(facts.WokenAt) || spriteFactValue(facts.WakeFailedAt) != "") {
					t.Fatalf("successful wake facts=%+v", facts)
				}
				if tt.wantWoken == 0 && !wakeTimeInRange(facts.WakeFailedAt) {
					t.Fatalf("failed wake facts=%+v", facts)
				}
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

func TestSpriteWakeSecret(t *testing.T) {
	t.Parallel()
	const other = "detent-test/other/token/value"
	for _, tt := range []struct {
		name, projectToken, organizationToken string
		member                                bool
		memberProject                         bool
		provider                              string
		want                                  string
	}{
		{name: "manual Sprite uses the target project token", projectToken: spritesSecretSentinel, organizationToken: other, want: spritesSecretSentinel},
		{name: "manual Sprite inherits the organization token", organizationToken: spritesSecretSentinel, want: spritesSecretSentinel},
		{name: "pool member keeps its organization token over a project override", projectToken: other, organizationToken: spritesSecretSentinel, member: true, provider: "detent-test", want: spritesSecretSentinel},
		{name: "pool member keeps its project token", projectToken: spritesSecretSentinel, organizationToken: other, member: true, memberProject: true, provider: "detent-test", want: spritesSecretSentinel},
		{name: "pool member from another Fly organization stays asleep", organizationToken: spritesSecretSentinel, member: true, provider: "elsewhere"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, scope, runners := newSpriteWakeFixture(t, http.DefaultClient, false, time.Minute, "customer-host")
			if tt.projectToken != "" {
				storeSpriteWakeSecret(t, f, tt.projectToken)
			}
			if tt.organizationToken != "" {
				storeOrganizationSpritesSecret(t, f, tt.organizationToken)
			}
			if tt.member {
				tokenProject := ""
				if tt.memberProject {
					tokenProject = string(f.project.ID)
				}
				setPlacementFixture(t, f, policy.Placement{Mode: "blended"}, 2)
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
					now := f.service.config.now()
					return w.CreateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: scope.organization, Name: "customer-host", ProviderOrganization: tt.provider, TokenProjectID: tracker.ProjectID(tokenProject), EnrollmentID: runners[0].enrollment.ID, State: "enrolled", IdleSince: now, CreatedAt: now})
				}); err != nil {
					t.Fatal(err)
				}
			}
			secretScope, _, envelope, err := f.service.spriteWakeSecretRecord(t.Context(), scope, "customer-host")
			if tt.want == "" {
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("err = %v, want no usable token", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			token, err := f.service.config.SecretKeys.Open(envelope, secretAAD(string(secretScope.organization), string(secretScope.project), flySpritesToken))
			if err != nil {
				t.Fatal(err)
			}
			if string(token) != tt.want {
				t.Fatal("wake used the wrong Sprites token")
			}
		})
	}
}

func spritePostgresFixtureConnection(t *testing.T, f nativeFixture) *pgx.Conn {
	t.Helper()
	config, err := f.service.config.Postgres.Connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pgx.ConnectConfig(t.Context(), config.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubReadTimeout)
		defer cancel()
		if err := connection.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := connection.Exec(t.Context(), "SELECT set_config('detent.organization_id', $1, false)", string(f.project.OrganizationID)); err != nil {
		t.Fatal(err)
	}
	return connection
}

func newSpriteWakeFixture(t *testing.T, client *http.Client, storeToken bool, heartbeatAgo time.Duration, names ...string) (nativeFixture, nativeScope, []runnerFixture) {
	t.Helper()
	f := newSpriteNativeFixture(t, Config{SecretKeys: secretTestKeys(t, "1", "1"), SpritesHTTPClient: client, RunDetails: &verifiedAttemptDetails{}})
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("fixture authority=%d %v", status, err)
	}
	f.service.wakeSpriteRunnersAfter = func(scope nativeScope, result json.RawMessage) {
		f.service.scheduleSpriteWake(t.Context(), scope, result)
	}
	f.service.startSpriteLifecycle = func(scope nativeScope, state string) { f.service.scheduleSpriteLifecycle(t.Context(), scope, state) }
	f.service.startSpritePoolForQueue = func(scope nativeScope) { f.service.scheduleSpritePoolForQueue(t.Context(), scope) }
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
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
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			identity, err := w.RunnerIdentity(t.Context(), runner.binding.RunnerID)
			if err != nil {
				return err
			}
			identity.LastHeartbeatAt = f.service.config.now().Add(-heartbeatAgo)
			return w.UpdateRunnerIdentity(t.Context(), identity)
		}); err != nil {
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
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		now := f.service.config.now()
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Kind: flySpritesToken, OrganizationSlug: "detent-test", Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Actor: "fixture", Kind: flySpritesToken, Event: "set", KeyVersion: envelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
}

func waitSpriteWake(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-testenv.WaitContext(t, t.Context()).Done():
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
			f, scope, runners := newSpriteWakeFixture(t, client, true, time.Minute, "customer-host")
			now := f.service.config.now()
			f.service.config.now = func() time.Time { return now }
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
			if test.status != http.StatusOK || test.transportError {
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
					operation, err := r.(postgres.SpriteReader).SpriteEnrollmentWake(t.Context(), runners[0].enrollment.ID)
					if err == nil {
						now = operation.NextAttemptAt
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				f.create(t, "due-retry")
				f.service.spriteWakeWork.Wait()
			}
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
			now = now.Add(2 * time.Minute)
			writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
				identity, err := w.RunnerIdentity(t.Context(), runners[0].binding.RunnerID)
				if err != nil {
					return err
				}
				identity.LastHeartbeatAt = now.Add(-time.Minute)
				return w.UpdateRunnerIdentity(t.Context(), identity)
			})
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
			if test.change == "rotation" {
				storeSpriteWakeSecret(t, f, spritesSecretSentinel+"-rotated")
			} else {
				writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), id)
					if err != nil {
						return err
					}
					switch test.change {
					case "grant":
						return w.DeleteAccessTokenGrant(t.Context(), identity.TokenID, scope.project)
					case "token":
						return w.RevokeAccessCredential(t.Context(), identity.TokenID, f.service.config.now())
					case "runner", "heartbeat":
						if test.change == "runner" {
							identity.State = "disabled"
							return w.UpdateRunnerRouting(t.Context(), identity.Revision, identity)
						} else {
							identity.LastHeartbeatAt = f.service.config.now()
						}
						return w.UpdateRunnerIdentity(t.Context(), identity)
					case "secret":
						return w.DeleteAccessSecret(t.Context(), scope.project, flySpritesToken, accessstore.SecretAudit{OrganizationID: scope.organization, ProjectID: scope.project, Actor: scope.credential.ID, Kind: flySpritesToken, Event: "delete", RecordedAt: f.service.config.now()})
					case "workflow":
						project, err := w.ProjectRecord(t.Context(), scope.project)
						if err != nil {
							return err
						}
						for i := range project.States {
							project.States[i].Dispatchable = false
						}
						_, err = w.UpdateProjectRecord(t.Context(), project)
						return err
					}
					return nil
				})
			}
			close(release)
			select {
			case got := <-result:
				if got.err != nil || got.woken != test.want || len(requests) != test.want {
					t.Fatalf("woken=%d requests=%d error=%v; want %d", got.woken, len(requests), got.err, test.want)
				}
			case <-testenv.WaitContext(t, t.Context()).Done():
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
			connection := spritePostgresFixtureConnection(t, f)
			if err := connection.QueryRow(t.Context(), "SELECT count(*) FROM project_secret_audit WHERE organization_id=$1 AND project_id=$2 AND event='use'", scope.organization, scope.project).Scan(&audits); err != nil || audits != test.want {
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
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		now := f.service.config.now()
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: f.project.OrganizationID, Kind: flySpritesToken, OrganizationSlug: "detent-test", Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: f.project.OrganizationID, Actor: "fixture", Kind: flySpritesToken, Event: "set", KeyVersion: envelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWakeManagedSpriteMembers(t *testing.T) {
	for _, test := range []struct {
		name         string
		ready, drain bool
		want         int
	}{
		{name: "verified sleeping member wakes for its project", ready: true, want: 1},
		{name: "member awaiting logins stays asleep", ready: false},
		{name: "draining member stays asleep", ready: true, drain: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			client := &http.Client{Transport: spritesTestTransport(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return spritesTestResponse(`{"type":"complete"}`, http.StatusOK), nil
			})}
			f, scope, _ := newSpriteWakeFixture(t, client, true, 0)
			keys := f.service.config.SecretKeys
			f.service.config.SecretKeys = nil
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			sprite := placementFixtureRunner(t, f, "managed-sprite", "sprite", true, 1, "gpt-6.1-sol")
			f.create(t, "managed work")
			setPlacementFixture(t, f, policy.Placement{Mode: "blended"}, 2)
			setSpriteAllocationFixture(t, f, 1, 0)
			now := f.service.config.now()
			writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
				return w.CreateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: scope.organization, AllocationProjectID: scope.project, TokenProjectID: scope.project, Name: sprite.redemption.Hostname, ProviderOrganization: "detent-test", EnrollmentID: sprite.enrollment.ID, State: "enrolled", IdleSince: now, CreatedAt: now})
			})
			if test.ready {
				setSpriteMemberChecks(t, f, spriteMemberReady)
			}
			if test.drain {
				updateSpriteMembersFixture(t, f, func(member *runnerstore.SpriteMembership) { member.State = "deleting" })
			}
			writeSpriteFixture(t, f, func(w tenantstore.Writer) error {
				identity, err := w.RunnerIdentity(t.Context(), sprite.binding.RunnerID)
				if err != nil {
					return err
				}
				if test.drain {
					identity.State = "draining"
				}
				identity.LastHeartbeatAt = now.Add(-time.Minute)
				if err := w.UpdateRunnerIdentity(t.Context(), identity); err != nil {
					return err
				}
				if test.drain {
					return w.UpdateRunnerRouting(t.Context(), identity.Revision, identity)
				}
				return nil
			})
			f.service.config.SecretKeys = keys
			woken, err := f.service.wakeOrganizationSpriteRunners(t.Context(), nativeScope{organization: scope.organization})
			if err != nil || woken != test.want || int(calls.Load()) != test.want {
				t.Fatalf("woken=%d calls=%d err=%v, want %d", woken, calls.Load(), err, test.want)
			}
		})
	}
}
