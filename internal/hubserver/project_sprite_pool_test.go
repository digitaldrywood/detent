package hubserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

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
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := decideSpriteScale(test.input); got != test.want {
				t.Fatalf("decision = %+v, want %+v", got, test.want)
			}
		})
	}
}

type spritePoolProvider struct {
	mu               sync.Mutex
	f                nativeFixture
	token            string
	fail             string
	created, deleted []string
	runners          []runnerauth.Redemption
	checkpoint       int
	started          chan struct{}
	deleteStarted    chan struct{}
	releaseDelete    chan struct{}
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
		if !strings.Contains(string(script), provider.token) || strings.Contains(r.URL.RawQuery, provider.token) {
			t.Error("fresh enrollment must travel only through stdin")
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
		name := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/sprites/"), "/")[0]
		binding := runnerauth.NewBinding()
		credential, err := apikey.GenerateToken()
		if err != nil {
			t.Error(err)
			return
		}
		redemption := runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: name, SpriteName: name, DisplayName: name, Capacity: 1, Version: "fixture"}
		response := performHubAPIRequest(t, provider.f.service, http.MethodPost, "/api/v2/organizations/"+string(provider.f.project.OrganizationID)+"/runner-enrollments/redeem", provider.token, redemption)
		if response.Code != http.StatusCreated {
			t.Errorf("bootstrap enrollment = %d: %s", response.Code, response.Body.String())
			return
		}
		provider.mu.Lock()
		provider.runners = append(provider.runners, redemption)
		provider.mu.Unlock()
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
	f, scope, _ := newSpriteWakeFixture(t, client, true, 0)
	f.create(t, "first")
	f.create(t, "second")
	f.service.spriteWakeWork.Wait()
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	f.service.config.Hosted = &HostedConfig{PublicURL: "https://hub.example.test"}
	f.service.config.Version = "v0.117.99"
	f.service.config.generateToken = func() (string, error) {
		token, err := apikey.GenerateToken()
		provider.token = token
		return token, err
	}
	provider.f = f
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO project_sprite_pools(organization_id,project_id,min_runners,max_runners,idle_seconds,bootstrap,configured_by) SELECT ?,?,0,2,300,'true',id FROM api_tokens WHERE token_hash=?`, scope.organization, scope.project, apikey.HashToken(testHubAdminToken)); err != nil {
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
	}{
		{"two independent connected members then idle deletion", "", 2, 2, 2, false},
		{"project heartbeat triggers idle cleanup", "", 2, 2, 2, true},
		{"bootstrap failure deletes only its member", "bootstrap", 1, 1, 0, false},
		{"checkpoint failure deletes enrolled member", "checkpoint", 1, 1, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, test.failure)
			var err error
			if test.failure == "" {
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
				if strings.Contains(member.BootstrapLog, provider.token) || strings.Contains(member.BootstrapLog, "private-provider-secret") || strings.Contains(member.BootstrapLog, spritesSecretSentinel) {
					t.Fatal("secret appeared in the retained bootstrap log")
				}
				if test.failure == "" && (member.State != "enrolled" || member.RunnerID == "") {
					t.Fatalf("successful member = %+v", member)
				}
			}
			if test.failure == "" {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET archived=1 WHERE project_id=?`, scope.project); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE project_sprite_members SET idle_since=? WHERE project_id=?`, formatHubTime(f.service.config.now().Add(-time.Hour)), scope.project); err != nil {
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

func TestSpritePoolCapacity(t *testing.T) {
	for _, test := range []struct {
		name                            string
		runners                         int
		reports, paused, wake, separate bool
		wantFree, wantPending           int
		requiredTag                     string
		grantTag                        bool
	}{
		{name: "unauthenticated enrollment is not ready", runners: 1},
		{name: "provider bounds reported local capacity", runners: 1, reports: true, wantFree: 1},
		{name: "unmatched project tag is not free capacity", runners: 1, reports: true, requiredTag: "sprite"},
		{name: "authorized project tag contributes capacity", runners: 1, reports: true, requiredTag: "sprite", grantTag: true, wantFree: 1},
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
				if test.grantTag {
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET tags_json=? WHERE id=?`, `["sprite"]`, r.binding.RunnerID); err != nil {
						t.Fatal(err)
					}
				}
				if test.reports {
					report := capacityReport(f.service.config.now())
					if test.separate {
						report.SharedAccountAlias = string(rune('a' + i))
					}
					data, err := json.Marshal([]providercapacity.Report{report})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET provider_reports_json=? WHERE id=?`, string(data), r.binding.RunnerID); err != nil {
						t.Fatal(err)
					}
				}
				if test.paused {
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?`, formatHubTime(f.service.config.now().Add(-time.Minute)), r.binding.RunnerID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.requiredTag != "" {
				descriptor := hubTestPolicy()
				descriptor.Requirements.RequiredTags = []string{test.requiredTag}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: hubTestPolicy().ID, Policy: descriptor.WithID()}), http.StatusOK)
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
			var err error
			switch test.change {
			case "secret":
				_, err = f.service.database.db.ExecContext(t.Context(), `DELETE FROM project_secrets WHERE project_id=?`, scope.project)
			case "actor":
				_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE api_tokens SET revoked_at=? WHERE id=?`, formatHubTime(f.service.config.now()), bootstrapTokenID)
			case "disable":
				_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE project_sprite_pools SET max_runners=0 WHERE project_id=?`, scope.project)
			case "project":
				scope.project = "prj_foreign"
			}
			if err != nil {
				t.Fatal(err)
			}
			err = f.service.scaleSpritePool(t.Context(), scope, true)
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
	case <-time.After(10 * time.Second):
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
	}{
		{"active host lease survives", "busy", 1, false},
		{"updated floor is preserved", "floor", 1, false},
		{"idle threshold is respected", "recent", 0, false},
		{"expired interrupted bootstrap is deleted by the existing lifecycle", "expired", 2, false},
		{"token from another provider organization cannot delete members", "organization", 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "")
			if err := f.service.scaleSpritePool(t.Context(), scope, true); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET archived=1 WHERE project_id=?`, scope.project); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE project_sprite_members SET idle_since=? WHERE project_id=?`, formatHubTime(f.service.config.now().Add(-time.Hour)), scope.project); err != nil {
				t.Fatal(err)
			}
			var err error
			switch test.change {
			case "busy":
				now := formatHubTime(f.service.config.now())
				_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,created_at,updated_at) SELECT 'lease-busy',i.id,r.machine_id,'session-busy',?,?,?,?,? FROM issues i JOIN runner_identities r ON r.organization_id=i.organization_id WHERE i.project_id=? LIMIT 1`, formatHubTime(f.service.config.now().Add(time.Hour)), now, now, now, now, scope.project)
			case "floor":
				_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE project_sprite_pools SET min_runners=1 WHERE project_id=?`, scope.project)
			case "recent":
				_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE project_sprite_members SET idle_since=? WHERE project_id=?`, formatHubTime(f.service.config.now()), scope.project)
			case "expired":
				_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE project_sprite_members SET state='bootstrapping' WHERE project_id=?`, scope.project)
				if err == nil {
					_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE runner_enrollments SET expires_at=? WHERE id IN (SELECT enrollment_id FROM project_sprite_members WHERE project_id=?)`, formatHubTime(f.service.config.now().Add(-time.Minute)), scope.project)
				}
			case "organization":
				_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE project_secrets SET organization_slug='another-org' WHERE project_id=?`, scope.project)
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
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET archived=1 WHERE project_id=?`, scope.project); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE project_sprite_members SET idle_since=? WHERE project_id=?`, formatHubTime(f.service.config.now().Add(-time.Hour)), scope.project); err != nil {
		t.Fatal(err)
	}
	provider.deleteStarted = make(chan struct{})
	provider.releaseDelete = make(chan struct{})
	f.service.maintainSpritePools(t.Context())
	waitSpriteWake(t, provider.deleteStarted)
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET archived=0 WHERE project_id=?`, scope.project); err != nil {
		t.Fatal(err)
	}
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

func TestSpritePoolClaimableDemand(t *testing.T) {
	for _, kind := range []string{"ordinary", "nondispatchable", "intake pending", "workspace", "answered"} {
		t.Run(kind, func(t *testing.T) {
			f, scope, _ := newSpritePoolFixture(t, "")
			now := formatHubTime(f.service.config.now())
			var err error
			switch kind {
			case "nondispatchable":
				_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE workflow_states SET dispatchable=0 WHERE project_id=?`, scope.project)
			case "intake pending":
				_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO github_imports(id,project_id,issue_number,work_item_id,intake_pending,observed_at) SELECT 'import_'||id,project_id,number,native_id,1,? FROM issues WHERE project_id=?`, now, scope.project)
			case "workspace":
				_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO workspace_sessions(id,organization_id,project_id,subject_work_item_id,ref,state,idle_timeout_seconds,expires_at,requested_expires_at,created_by,created_at,updated_at) SELECT 'workspace_'||id,organization_id,project_id,native_id,'ref','requested',300,?,?, 'fixture',?,? FROM issues WHERE project_id=?`, now, now, now, now, scope.project)
				if err == nil {
					_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO workspace_items(work_item_id,workspace_id,organization_id,project_id,created_at) SELECT native_id,'workspace_'||id,organization_id,project_id,? FROM issues WHERE project_id=?`, now, scope.project)
				}
			case "answered":
				hosted := f.service.config.Hosted
				f.service.config.Hosted = nil
				worker := f.worker(t, "answers")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "machine_pool_answer", "hostname": "answer", "capacity": 2, "version": "test"}), http.StatusOK)
				_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) SELECT 'lease_answer_'||id,id,'machine_pool_answer','session_answer_'||id,?,?,?,?,?,? FROM issues WHERE project_id=?`, now, now, now, now, now, now, scope.project)
				f.service.config.Hosted = hosted
				if err == nil {
					_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,checkpoint_json,work_item_revision,dispatch_generation,started_at,updated_at) SELECT 'attempt_answer_'||i.id,i.organization_id,i.project_id,i.native_id,l.lease_id,l.fencing_token,'run_answer_'||i.id,1,'succeeded','{}','{"worktree_state":"clean"}',i.revision,i.dispatch_generation,?,? FROM issues i JOIN leases l ON l.issue_id=i.id WHERE i.project_id=?`, now, now, scope.project)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			view, err := f.service.readSpritePool(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			input, _, err := f.service.spritePoolSnapshot(t.Context(), scope, view)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if kind == "ordinary" {
				want = 2
			}
			if input.Depth != want || decideSpriteScale(input).Create != want {
				t.Fatalf("demand %+v decision %+v, want %d", input, decideSpriteScale(input), want)
			}
		})
	}
}

func TestSpritePoolTargeting(t *testing.T) {
	for _, test := range []struct {
		name         string
		requirements policy.Requirements
		want         int
	}{
		{"ordinary project", policy.Requirements{}, 2},
		{"authorized sprite tag", policy.Requirements{RequiredTags: []string{"sprite"}}, 2},
		{"ungranted privileged tag", policy.Requirements{RequiredTags: []string{"production"}}, 0},
		{"pinned existing runner", policy.Requirements{RunnerID: "runner_existing"}, 0},
		{"pinned existing host", policy.Requirements{MachineID: "machine_existing"}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, scope, provider := newSpritePoolFixture(t, "")
			descriptor := hubTestPolicy()
			descriptor.Requirements = test.requirements
			hosted := f.service.config.Hosted
			f.service.config.Hosted = nil
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: hubTestPolicy().ID, Policy: descriptor.WithID()}), http.StatusOK)
			f.service.config.Hosted = hosted
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE project_sprite_pools SET min_runners=1 WHERE project_id=?`, scope.project); err != nil {
				t.Fatal(err)
			}
			view, err := f.service.readSpritePool(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			input, _, err := f.service.spritePoolSnapshot(t.Context(), scope, view)
			if err != nil {
				t.Fatal(err)
			}
			if got := decideSpriteScale(input).Create; got != test.want {
				t.Fatalf("targeted input %+v creates %d, want %d", input, got, test.want)
			}
			if test.want == 0 {
				if err := f.service.scaleSpritePool(t.Context(), scope, true); err != nil {
					t.Fatal(err)
				}
				if len(provider.created) != 0 {
					t.Fatal("ineligible policy created a provider resource")
				}
			}
		})
	}
}

func TestSpritePoolUnsupportedVersionHasNoSideEffects(t *testing.T) {
	f, scope, provider := newSpritePoolFixture(t, "")
	f.service.config.Version = "develop-51be10a"
	view, err := f.service.readSpritePool(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	var before int
	if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT count(*) FROM runner_enrollments`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := f.service.createPoolSprite(t.Context(), scope, view.spritePoolSettings); err == nil {
		t.Fatal("unsupported build must retain pinned-release refusal")
	}
	var after, members int
	if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT count(*) FROM runner_enrollments`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT count(*) FROM project_sprite_members`).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if after != before || members != 0 || len(provider.created) != 0 || len(provider.deleted) != 0 {
		t.Fatalf("unsupported build created side effects: enrollments %d -> %d, members %d, provider create/delete %d/%d", before, after, members, len(provider.created), len(provider.deleted))
	}
}
