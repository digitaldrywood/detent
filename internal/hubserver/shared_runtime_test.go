package hubserver

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"go.uber.org/goleak"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSharedHostedRuntime(t *testing.T) {
	ctx := t.Context()
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	base := startupConfig(connection)
	host, err := OpenHostedRuntime(ctx, *base.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := host.Close(); err != nil {
			t.Error(err)
		}
	})
	var services []*Service
	var ids []tracker.OrganizationID
	key := hostedSharedKey(19)
	open := func(total int) {
		t.Helper()
		for len(services) < total {
			id := tracker.OrganizationID(fmt.Sprintf("org_example%02d", len(services)))
			cfg := startupConfig(connection)
			cfg.SecretKeys = secretTestKeys(t, "1", "1")
			cfg.Hosted.OrganizationID = string(id)
			cfg.Hosted.WorkOSOrganizationID = "provider_" + string(id)
			cfg.Hosted.SharedEntry = &hostedplans.HostedSharedEntry{Issuer: "entry", Generation: 1, PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}}
			cfg.Conversation = &ConversationConfig{Enabled: true}
			if _, err := admin.ExecContext(ctx, `INSERT INTO platform.organizations(id,provider_id,name,state,endpoint,generation,created_at,updated_at) VALUES ($1,$2,$1,'ready',$1,1,now(),now())`, id, cfg.Hosted.WorkOSOrganizationID); err != nil {
				t.Fatal(err)
			}
			if err := host.ProvisionTenant(ctx, id, string(id), fmt.Sprintf("example-%02d", len(services)), cfg); err != nil {
				t.Fatal(err)
			}
			service, err := host.OpenTenant(ctx, id, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
			services = append(services, service)
			ids = append(ids, id)
			if err := service.database.postgres.WriteTenant(ctx, id, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: id, ID: "prj_same", Name: string(id), Slug: "example", Profile: "native"}, States: nativeFixtureStates()})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	type project struct{ ID, Name string }
	request := func(destination int, audience int) (int, []project) {
		t.Helper()
		id, err := cloudassert.NewID()
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		const target = "/internal/v1/platform/projects"
		claims := cloudassert.Claims{Issuer: "entry", Audience: string(ids[audience]), Generation: 1, Kind: cloudassert.KindService, Method: http.MethodPost, Path: target, BodyDigest: cloudassert.BodyDigest([]byte("{}")), IssuedAt: now, ExpiresAt: now.Add(30 * time.Second), ID: id}
		token, err := cloudassert.Sign(key, claims)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader("{}"))
		r.Header.Set(cloudassert.Header, token)
		resp := httptest.NewRecorder()
		services[destination].Handler().ServeHTTP(resp, r)
		var rows []project
		if resp.Code == http.StatusOK {
			if err := json.Unmarshal(resp.Body.Bytes(), &rows); err != nil {
				t.Fatal(err)
			}
		}
		return resp.Code, rows
	}
	idle := goleak.IgnoreCurrent()
	measure := func(total int) int {
		t.Helper()
		open(total)
		for i := range 100 {
			index := i % total
			status, rows := request(index, index)
			want := []project{{"prj_same", string(ids[index])}}
			if status != http.StatusOK || !slices.Equal(rows, want) {
				t.Fatalf("organization %d projects = %v, HTTP %d", index, rows, status)
			}
		}
		stats := host.database.postgres.Stats()
		if stats.TotalConns() != 1 || stats.AcquiredConns() != 0 {
			t.Fatalf("%d organizations: open %d acquired %d", total, stats.TotalConns(), stats.AcquiredConns())
		}
		var pid, backends int
		if err := admin.QueryRowContext(ctx, "SELECT min(pid), count(*) FROM pg_stat_activity WHERE usename=$1 AND datname=current_database()", connection.User).Scan(&pid, &backends); err != nil {
			t.Fatal(err)
		}
		if backends != 1 {
			t.Fatalf("runtime backends = %d, want 1", backends)
		}
		t.Logf("%d organizations, 100 sequential requests, %d open connections, backend %d", total, stats.TotalConns(), pid)
		return pid
	}
	pid := measure(2)
	stale := services[0].config
	staleHosted := *stale.Hosted
	staleEntry := *staleHosted.SharedEntry
	staleEntry.Generation++
	staleHosted.SharedEntry = &staleEntry
	stale.Hosted = &staleHosted
	if service, err := host.OpenTenant(ctx, ids[0], stale); err == nil {
		t.Fatal("stale tenant opened", service.Close())
	}

	if status, _ := request(1, 0); status != http.StatusUnauthorized {
		t.Fatalf("foreign assertion returned %d", status)
	}
	if err := services[1].database.postgres.ReadTenant(ctx, ids[0], corestore.Serving, func(tenantstore.Reader) error { t.Fatal("foreign transaction admitted"); return nil }); !errors.Is(err, pgtransport.ErrUnbound) {
		t.Fatalf("foreign transaction = %v", err)
	}
	if large := measure(50); large != pid {
		t.Fatalf("sequential requests switched PostgreSQL backends from %d to %d", pid, large)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	releaseHolder := sync.OnceFunc(func() { close(release) })
	defer releaseHolder()
	holder := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
		return services[0].database.postgres.ReadTenant(ctx, ids[0], corestore.Serving, func(r tenantstore.Reader) error {
			close(held)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})
	pgstoretest.WaitRepositorySignal(t, held)
	checkSecond := func(ctx context.Context) error {
		return services[1].database.postgres.ReadTenant(ctx, ids[1], corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.Projects(ctx, "", 2)
			if err == nil && (len(rows) != 1 || rows[0].OrganizationID != ids[1]) {
				return errors.New("concurrent read returned another organization's project")
			}
			return err
		})
	}
	attempting := make(chan struct{})
	queued := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
		close(attempting)
		return checkSecond(ctx)
	})
	pgstoretest.WaitRepositorySignal(t, attempting)
	background := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
		return checkSecond(postgres.WithBackgroundWork(ctx))
	})
	backgroundErr := background.Wait(t)
	select {
	case <-queued.Done:
		t.Fatal("second organization bypassed shared serving admission")
	default:
	}
	releaseHolder()
	if err := errors.Join(backgroundErr, holder.Wait(t), queued.Wait(t)); err != nil {
		t.Fatal(err)
	}
	if n := host.database.postgres.Stats().AcquiredConns(); n != 0 {
		t.Fatalf("concurrent work retained %d connections", n)
	}
	for _, service := range services[:2] {
		select {
		case <-service.database.dashboardCommitted:
		default:
		}
	}
	if err := services[0].Close(); err != nil {
		t.Fatal(err)
	}
	if err := services[1].database.postgres.WriteTenant(ctx, ids[1], corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: ids[1], ID: "prj_after", Name: "second", Slug: "second", Profile: "native"}, States: nativeFixtureStates()})
		if err != nil {
			return err
		}
		_, err = w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: ids[1], ProjectID: "prj_after", NativeID: "item_example", Number: 1, Title: "Example"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-services[1].database.dashboardCommitted:
	default:
		t.Fatal("remaining organization lost its board commit notification")
	}
	select {
	case <-services[0].database.dashboardCommitted:
		t.Fatal("closed organization received a foreign notification")
	default:
	}
	if status, rows := request(1, 1); status != http.StatusOK || !slices.Equal(rows, []project{{"prj_same", string(ids[1])}, {"prj_after", "second"}}) {
		t.Fatalf("remaining organization projects = %v, HTTP %d", rows, status)
	}
	goleak.VerifyNone(t, idle)
	for _, service := range services {
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	assertStartupDisconnected(t, admin, connection)
}

func TestHostedRunnerBudgetSurvivesIdleViewRetirement(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 15, 0, time.UTC)
	host := &HostedRuntime{database: &database{postgres: &postgres.Repository{}}}
	open := func(org tracker.OrganizationID, generation int64) *Service {
		t.Helper()
		s := &Service{database: host.database, runtimeHost: host, config: Config{TenantRepository: host.database.postgres,
			Logger: slog.New(slog.DiscardHandler), Clock: func() time.Time { return now },
			Hosted: &hostedplans.HostedConfig{OrganizationID: string(org), SharedEntry: &hostedplans.HostedSharedEntry{Generation: generation}},
		}}
		m, err := s.openRequestMetrics()
		if err != nil {
			t.Fatal(err)
		}
		s.requestMetrics = m
		return s
	}
	admit := func(s *Service) int {
		return s.requestMetrics.Admit(now, "runner", "GET /items", 0, []string{"project"})
	}
	first := open("org_example", 1)
	for range 120 {
		if got := admit(first); got != 0 {
			t.Fatalf("within budget got retry-after %d", got)
		}
	}
	if first.HostedActivity() {
		t.Error("runner budget retained idle tenant view")
	}
	if ready, err := first.PrepareIdleClose(t.Context()); !ready || err != nil {
		t.Fatalf("ready to evict = %v, error = %v", ready, err)
	}
	reopened := open("org_example", 1)
	response := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/items", nil), response)
	c.SetPath("/items")
	credential := hubapi.APICredential{RequestCapacity: new(int)}
	credential.Runner.RunnerID = "runner"
	credential.Runner.ProjectIDs = []tracker.ProjectID{"project"}
	if err := reopened.admitRunnerRequest(c, credential); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "45" {
		t.Errorf("reopened request = HTTP %d, retry-after %q", response.Code, response.Header().Get("Retry-After"))
	}
	for _, other := range []*Service{open("org_other", 1), open("org_example", 2)} {
		if got := admit(other); got != 0 || len(other.requestMetrics.Findings(now)) != 0 {
			t.Error("runner budget crossed organization or generation")
		}
	}
	later := open("org_example", 1)
	findings := later.requestMetrics.Findings(now.Add(2 * time.Minute))
	if len(findings) != 1 || findings[0].Evidence.Counts["requests"] != 121 || !slices.Equal(findings[0].Projects, []string{"project"}) {
		t.Errorf("reopened findings = %+v", findings)
	}
	if got := later.requestMetrics.Findings(now.Add(2*time.Minute + time.Nanosecond)); len(got) != 0 {
		t.Errorf("expired findings = %+v", got)
	}
}
