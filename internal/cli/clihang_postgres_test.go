package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var clihangHubTemplate = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	roles, err := os.ReadFile(filepath.Join("..", "hubserver", "postgres", "roles.sql"))
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, string(roles)); err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "SET ROLE detent_migration_v2"); err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db,
		postgres.RoleMigrations(os.DirFS(filepath.Join("..", "hubserver", "postgres", "migrations"))),
		goose.WithGoMigrations(postgres.ConversationMigrations(os.DirFS(filepath.Join("..", "hubserver", "postgres", "migrations")))...), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
})

func clihangPostgresHub(t *testing.T, token string, content entryauthority.ContentHandler, options ...clihangPostgresOptions) (*hubserver.Service, func()) {
	t.Helper()
	admin := clihangHubTemplate.Database(t)
	connection := pgtransport.Connection{User: "example_" + strings.ReplaceAll(uuid.NewString(), "-", ""), Password: uuid.NewString(), SSLMode: "disable"}
	var address, directory string
	if err := admin.QueryRowContext(t.Context(), `SELECT current_database(), current_setting('port')::integer,
		COALESCE(inet_server_addr()::text, current_setting('unix_socket_directories')), current_setting('data_directory')`).Scan(
		&connection.Database, &connection.Port, &address, &directory); err != nil {
		t.Fatal(err)
	}
	connection.Host = address
	if address == "." {
		connection.Host = directory
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{connection.Database}.Sanitize()+" CONNECTION LIMIT 4"); err != nil {
		t.Fatal(err)
	}
	role := pgx.Identifier{connection.User}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+role+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(connection.Password)+" IN ROLE detent_runtime_v2"); err != nil {
		t.Fatal(err)
	}
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		if name == "DETENT_HUB_SECRET_KEYS" {
			return fmt.Sprintf(`{"1":%q}`, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32)))
		}
		return "1"
	})
	if err != nil {
		t.Fatal(err)
	}
	poolConfig, err := connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	budgets := postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}
	repository, err := postgres.NewRepository(t.Context(), pool, budgets)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	defer repository.Close()
	for _, organization := range []tracker.OrganizationID{"org_example", "org_other"} {
		now := time.Now().UTC().Truncate(time.Microsecond)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), Slug: strings.ReplaceAll(string(organization), "_", "-"), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_" + string(organization), AllocationGeneration: 1}); err != nil {
				return err
			}
			key := bytes.Repeat([]byte{0x5a}, 32)
			if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: "hub_" + artifact.Digest([]byte(organization))[:32], CursorKey: key}); err != nil {
				return err
			}
			envelope, err := keys.Seal(key, accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
			if err != nil {
				return err
			}
			if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{
				Core:   corestore.Project{OrganizationID: organization, ID: tracker.ProjectID("prj_" + string(organization)), Name: "Example project", Slug: "example", Profile: "native", CreatedAt: now},
				States: []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true}},
			}); err != nil {
				return err
			}
			credential := token
			if organization != "org_example" {
				credential += "-other"
			}
			user, membership, principal := "user_"+string(organization), "membership_"+string(organization), "admin_"+string(organization)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: principal, Name: "Example administrator", Hash: apikey.HashToken(credential), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "personal", UserID: user, MembershipID: membership, CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: user, Email: user + "@example.test", MembershipID: membership, Role: "owner", Active: true, PrincipalID: principal, CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			project := tracker.ProjectID("prj_" + string(organization))
			if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, ProjectID: project, UserID: user, CanWrite: true}); err != nil {
				return err
			}
			if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, ProjectID: project, TokenID: principal}); err != nil {
				return err
			}
			expires := now.Add(time.Hour)
			workerID, enrollmentID := "worker_"+string(organization), "enrollment_"+string(organization)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: workerID, Name: "Example worker", Hash: apikey.HashToken(credential + "-worker"), Scope: "worker", ProjectAccess: "selected", Kind: "service", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires}); err != nil {
				return err
			}
			if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, ProjectID: project, TokenID: workerID}); err != nil {
				return err
			}
			binding := clihangBinding(organization)
			operations := []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: enrollmentID, RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenHash: apikey.HashToken(enrollmentID), Scope: "projects", ProjectIDs: []tracker.ProjectID{project}, Operations: operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: principal}); err != nil {
				return err
			}
			if err := w.RedeemRunnerEnrollment(t.Context(), enrollmentID, binding.RunnerID, binding.MachineID, now); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: string(binding.MachineID), Hostname: "example", Capacity: 2, Version: "1.0.0"}); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenID: workerID, EnrollmentID: enrollmentID, Scope: "projects", State: "active", IsolationTier: string(isolation.NativeTrusted), Operations: operations, CapacityLimit: 2, ReportedCapacity: 2, ProjectIDs: []tracker.ProjectID{project}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
				return err
			}
			return w.UpdateRunnerCapacityConfig(t.Context(), binding.RunnerID, runnerauth.CapacityConfig{Revision: artifact.Digest([]byte(organization)), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, ObservedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			stored, err := r.AccessOrganization(t.Context())
			if err == nil && stored.OrganizationID != organization {
				return errors.New("fixture organization is not isolated")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, option := range options {
		if option.seed != nil {
			if err := option.seed(repository); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := admin.Close(); err != nil {
		t.Fatal(err)
	}
	provider, err := auth.NewHostedProvider(auth.IdentityProviderWorkOS, auth.WorkOSConfig{APIURL: "https://identity.example.test", ClientID: "client_example", APIKey: "fixture-key", RedirectURL: "https://hub.example.test/auth/callback", HTTPClient: &http.Client{Transport: readinessRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/user_management/organization_memberships" {
			return nil, errors.New("unexpected identity provider request")
		}
		organization := strings.TrimPrefix(r.URL.Query().Get("organization_id"), "provider_")
		membership := auth.Membership{ID: "membership_" + organization, UserID: "user_" + organization, OrganizationID: "provider_" + organization, Status: "active"}
		membership.Role.Slug = "owner"
		response := httptest.NewRecorder()
		if err := json.NewEncoder(response).Encode(map[string]any{"data": []auth.Membership{membership}}); err != nil {
			return nil, err
		}
		return response.Result(), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	config := hubserver.Config{DatabaseBackend: hubserver.DatabasePostgres,
		Postgres: &postgres.Config{Connection: connection, Pool: budgets}, SecretKeys: keys, Content: content,
		RunDetails: hubmaintenance.LimitDetails(&clihangDetails{data: make(map[string][]byte)}), Hosted: &hubserver.HostedConfig{
			OrganizationID: "org_example", WorkOSOrganizationID: "provider_org_example", PublicURL: "https://hub.example.test", Provider: provider,
		}}
	for _, option := range options {
		if option.details != nil {
			config.RunDetails = hubmaintenance.LimitDetails(option.details)
		}
	}
	for _, option := range options {
		if option.config != nil {
			*option.config = config
		}
	}
	service, err := hubserver.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	return service, func() {
		pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
		if err != nil {
			t.Fatal(err)
		}
		repository, err := postgres.NewRepository(t.Context(), pool, budgets)
		if err != nil {
			pool.Close()
			t.Fatal(err)
		}
		defer repository.Close()
		if err := repository.WriteTenant(t.Context(), "org_example", corestore.Serving, func(w tenantstore.Writer) error {
			return w.RevokeAccessCredential(t.Context(), "worker_org_example", time.Now().UTC())
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func clihangBinding(organization tracker.OrganizationID) runnerauth.Binding {
	id := artifact.Digest([]byte(organization))[:32]
	return runnerauth.Binding{RunnerID: "runner_" + id, MachineID: tracker.MachineID("machine_" + id)}
}

type clihangDetails struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (s *clihangDetails) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil || ctx.Err() != nil {
		return ref, errors.Join(err, ctx.Err())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[ref.Key] = bytes.Clone(data)
	return ref, nil
}

func (s *clihangDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := ref.Validate(organization); err != nil {
		return nil, err
	}
	return s.read(ctx, ref.Key)
}

func (s *clihangDetails) ReadCanonicalDetail(ctx context.Context, organization, project, key string) ([]byte, error) {
	if err := attachment.ValidateCanonicalDetailKey(organization, project, key); err != nil {
		return nil, err
	}
	return s.read(ctx, key)
}

func (s *clihangDetails) read(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, found := s.data[key]
	if !found {
		return nil, artifact.ErrMissing
	}
	return bytes.Clone(data), nil
}

type clihangPostgresOptions struct {
	details attachment.DetailStore
	seed    func(*postgres.Repository) error
	config  *hubserver.Config
}
