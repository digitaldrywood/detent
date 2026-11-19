package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestTenantHeartbeatHTTPCommandAndAuthority(t *testing.T) {
	t.Parallel()
	f := newRunnerPGFixture(t)
	connection := startupConnection(t, f.admin, "detent_runtime_v2", "")
	config := postgresStartupTemplate.NativeConfig(t, f.admin, connection.User, connection.Password)
	config.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	original := f.service.database.postgres
	t.Cleanup(func() {
		f.service.config.TenantRepository = original
		f.service.database.postgres = original
	})
	f.service.config.TenantRepository = repository
	f.service.database.postgres = repository
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE runner_identities SET display_name='Example' WHERE organization_id='org_startup'`); err != nil {
		t.Fatal(err)
	}
	for index, binding := range f.bindings {
		path := "/api/v2/organizations/org_startup/projects/prj_runner/machines/" + string(binding.MachineID) + "/heartbeat"
		request := nativeMachineHeartbeatRequest{DisplayName: "Example", Capacity: 1, Version: "0.183.0", ProtocolMajor: 1, OS: "linux", Architecture: "amd64", BackendIsolation: isolation.Report{"example": {isolation.NativeTrusted}}}
		response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], request)
		requireNativeStatus(t, response, http.StatusOK)
		var snapshot runnerauth.RoutingSnapshot
		if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.RunnerID != binding.RunnerID || snapshot.Revision != 1 {
			t.Fatalf("wrong snapshot: %+v", snapshot)
		}
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE api_tokens SET revoked_at=clock_timestamp() WHERE organization_id='org_startup' AND id=$1`, fmt.Sprintf("credential_%d", index+1)); err != nil {
			t.Fatal(err)
		}
		denied := performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], request)
		requireNativeStatus(t, denied, http.StatusUnauthorized)
	}
}
