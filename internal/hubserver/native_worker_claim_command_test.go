package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type claimRequestTraceKey struct{}

func TestNativeClaimReleaseHTTPWireIncludesAuthentication(t *testing.T) {
	f := newRunnerPGFixture(t)
	config, err := f.service.config.Postgres.Connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	pgtransport.Configure(config)
	config.MaxConns = 3
	wire := new(heartbeatWire)
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := new(net.Dialer).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &heartbeatConn{Conn: conn, wire: wire, startup: true}, nil
	}
	config.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		stream, ok := conn.PgConn().Conn().(*heartbeatConn)
		if !ok {
			return false, fmt.Errorf("missing wire wrapper: %T", conn.PgConn().Conn())
		}
		stream.wire = new(heartbeatWire)
		if ctx.Value(claimRequestTraceKey{}) == true {
			stream.wire = wire
		}
		return true, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, f.service.config.Postgres.Pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	original := f.service.database.postgres
	t.Cleanup(func() { f.service.config.TenantRepository = original; f.service.database.postgres = original })
	f.service.config.TenantRepository = repository
	f.service.database.postgres = repository

	for index, binding := range f.bindings {
		if err := repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			return w.UpdateRunnerCapacityConfig(t.Context(), binding.RunnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, ObservedAt: f.service.config.now().UTC()})
		}); err != nil {
			t.Fatal(err)
		}
		var version eventstore.CollaborationVersion
		if err := repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			version, err = r.CollaborationVersion(t.Context(), string(f.leases[index].WorkItemID), 1)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		content := eventstore.NewContent(f.service.runDetailStore())
		raw, err := content.CollaborationVersion(t.Context(), version)
		if err != nil {
			t.Fatal(err)
		}
		var issue tracker.NativeIssue
		if err := json.Unmarshal(raw, &issue); err != nil {
			t.Fatal(err)
		}
		issue.IssueContract = &issuecontract.State{Exempt: true}
		issue.Revision++
		version.Revision++
		raw, err = json.Marshal(issue)
		if err != nil {
			t.Fatal(err)
		}
		version, err = content.PrepareCollaborationVersion(t.Context(), version, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			item, err := w.WorkItem(t.Context(), "prj_runner", f.leases[index].WorkItemID)
			if err != nil {
				return err
			}
			if _, err := w.UpdateWorkItem(t.Context(), item); err != nil {
				return err
			}
			_, err = w.CreateCollaborationVersion(t.Context(), version)
			return err
		}); err != nil {
			t.Fatal(err)
		}

		claimPath := "/api/v2/organizations/org_startup/projects/prj_runner/claims"
		claim := tracker.NativeClaim{PolicyID: f.policy.PolicyID, WorkItemID: f.leases[index].WorkItemID, MachineID: binding.MachineID, SessionID: f.leases[index].SessionID, TTLSeconds: 300, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}}
		releasePath := "/api/v2/organizations/org_startup/projects/prj_runner/leases/" + string(f.leases[index].ID) + "/release"
		for _, operation := range []struct {
			name, path string
			body       any
			status     int
		}{
			{"claim replay", claimPath, claim, http.StatusOK},
			{"release", releasePath, tracker.NativeLeaseMutation{FencingToken: f.leases[index].FencingToken, Reason: "released"}, http.StatusNoContent},
		} {
			wire.Lock()
			wire.statements, wire.exchanges, wire.transactions = 0, 0, 0
			wire.Unlock()
			response := httptest.NewRecorder()
			request := newHubAPIRequest(t, http.MethodPost, operation.path, f.tokens[index], operation.body).WithContext(context.WithValue(t.Context(), claimRequestTraceKey{}, true))
			f.service.Handler().ServeHTTP(response, request)
			wire.Lock()
			statements, exchanges, transactions := wire.statements, wire.exchanges, wire.transactions
			wire.Unlock()
			requireNativeStatus(t, response, operation.status)
			if transactions == 0 || exchanges == 0 {
				t.Fatal("HTTP request escaped wire instrumentation")
			}
			t.Logf("runner=%d full HTTP %s: statements=%d exchanges=%d transactions=%d", index, operation.name, statements, exchanges, transactions)
		}
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE api_tokens SET revoked_at=clock_timestamp() WHERE organization_id='org_startup' AND id=$1`, fmt.Sprintf("credential_%d", index+1)); err != nil {
			t.Fatal(err)
		}
		wire.Lock()
		wire.statements, wire.exchanges, wire.transactions = 0, 0, 0
		wire.Unlock()
		denied := httptest.NewRecorder()
		deniedRequest := newHubAPIRequest(t, http.MethodPost, claimPath, f.tokens[index], claim).WithContext(context.WithValue(t.Context(), claimRequestTraceKey{}, true))
		f.service.Handler().ServeHTTP(denied, deniedRequest)
		requireNativeStatus(t, denied, http.StatusUnauthorized)
		wire.Lock()
		authExchanges, authTransactions := wire.exchanges, wire.transactions
		wire.Unlock()
		if authExchanges == 0 || authTransactions == 0 {
			t.Fatal("authentication escaped wire instrumentation")
		}
		t.Logf("runner=%d revoked authentication: exchanges=%d transactions=%d", index, authExchanges, authTransactions)
	}
}
