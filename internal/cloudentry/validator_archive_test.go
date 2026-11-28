package cloudentry

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestValidatorArchiveUploadChecksLeaseAtPublication(t *testing.T) {
	for _, tc := range []struct {
		name              string
		revoke, atReserve bool
	}{
		{name: "expires before quota reservation", atReserve: true},
		{name: "expires during object upload"},
		{name: "revoked during object upload", revoke: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSeparateDatabaseEntryFixture(t, slog.New(slog.DiscardHandler))
			if err := f.service.closeAllocator(); err != nil {
				t.Fatal(err)
			}
			tenant := f.fixtures["org_alpha"]
			org := tracker.OrganizationID(tenant.ID)
			migrations, err := postgres.MigrationFiles()
			if err != nil {
				t.Fatal(err)
			}
			repo, err := postgres.Open(t.Context(), *tenant.Config.Postgres, migrations)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := repo.Close(); err != nil {
					t.Error(err)
				}
			})
			leased := hubfixture.SeedLeasedRunnersThrough(t, &hubfixture.Hub{Service: tenant.Service, Config: tenant.Config, Admin: tenant.Database, Repository: repo, Primary: org}, org, f.service.Handler())
			lease := leased.Leases[0]
			base := "/api/v2/organizations/" + tenant.ID + "/projects/prj_runner/work-items/"
			store := newSpacesFixture(t, false)
			armed, ended := false, false
			endLease := func() error {
				ended = true
				if tc.revoke {
					return repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: string(lease.ID), FencingToken: int64(lease.FencingToken)}, "cancelled")
						return err
					})
				}
				_, err := tenant.Database.ExecContext(t.Context(), `UPDATE leases SET acquired_at=clock_timestamp()-interval '2 minutes',renewed_at=clock_timestamp()-interval '2 minutes',expires_at=clock_timestamp()-interval '1 minute' WHERE organization_id=$1 AND lease_id=$2`, org, lease.ID)
				return err
			}
			puts := 0
			transport := archiveValidationTransport{next: store.Transport, check: func(req *http.Request) error {
				if req.Method != http.MethodPut || !strings.Contains(req.URL.Path, "/blobs/sha256/") {
					return nil
				}
				puts++
				if armed && !ended && !tc.atReserve {
					return endLease()
				}
				return nil
			}}
			if tc.atReserve {
				original := f.service.config.Transport
				checks := 0
				path := "/api/v2/organizations/" + tenant.ID + "/projects/prj_runner/attachment-metadata/archives/" + string(lease.WorkItemID) + "/check"
				f.service.config.Transport = func(organization Organization) (http.RoundTripper, error) {
					next, err := original(organization)
					return archiveValidationTransport{next: next, check: func(req *http.Request) error {
						if armed && !ended && req.Method == http.MethodPost && req.URL.Path == path {
							checks++
							if checks == 2 {
								return endLease()
							}
						}
						return nil
					}}, err
				}
			}
			cfg := store.Config()
			cfg.Endpoint = "https://objects.example.test"
			storage, err := attachment.NewStorage(t.Context(), cfg, transport)
			if err != nil {
				t.Fatal(err)
			}
			f.service.attachments = storage
			f.service.config.Attachments = &cfg
			upload := func(index int, item tracker.NativeWorkItemID, key string, includeFence bool) *httptest.ResponseRecorder {
				t.Helper()
				lease := leased.Leases[index]
				body := changerequest.ValidatorArchive{OrganizationID: org, ProjectID: "prj_runner", WorkItemID: item, ChangeID: "change_example", VersionID: "version_example", Review: tracker.ReviewChange{
					Mutation:  tracker.Mutation{IdempotencyKey: key, LeaseID: lease.ID, FencingToken: lease.FencingToken},
					Validator: &gate.ValidatorResult{VersionID: "version_example", Verdict: "pass"},
				}}
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, base+string(item)+"/archives", jsonBody(t, body))
				req.Header.Set("Authorization", "Bearer "+leased.Tokens[index])
				req.Header.Set("X-Detent-Archive-Schema", "1")
				req.Header.Set("Content-Type", "application/json")
				if includeFence {
					req.Header.Set(attachment.ArchiveLeaseHeader, string(lease.ID))
					req.Header.Set(attachment.ArchiveFenceHeader, strconv.FormatInt(int64(lease.FencingToken), 10))
				}
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, req)
				return response
			}
			otherItem := leased.Leases[1].WorkItemID
			accepted := upload(1, otherItem, "accepted", true)
			hubfixture.RequireStatus(t, accepted, http.StatusCreated)
			var retained attachment.ArchiveReference
			if err := json.Unmarshal(accepted.Body.Bytes(), &retained); err != nil {
				t.Fatal(err)
			}
			replay := upload(1, otherItem, "accepted", true)
			hubfixture.RequireStatus(t, replay, http.StatusCreated)
			if replay.Body.String() != accepted.Body.String() || puts != 1 {
				t.Fatal("replaying a published archive changed its reference or uploaded it again")
			}
			usage := func() int64 {
				t.Helper()
				var used int64
				if err := repo.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					used, err = r.CollaborationBytes(t.Context())
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return used
			}
			before := usage()
			armed = true
			rejected := upload(0, lease.WorkItemID, "interrupted", true)
			if !ended {
				t.Fatalf("upload did not reach the lease-loss boundary: %d %s", rejected.Code, rejected.Body.String())
			}
			hubfixture.RequireCode(t, rejected, http.StatusConflict, "stale_fencing_token")
			hubfixture.RequireCode(t, upload(0, lease.WorkItemID, "retry", true), http.StatusConflict, "stale_fencing_token")
			if response := upload(0, lease.WorkItemID, "missing-fence", false); response.Code < 400 || response.Code >= 500 {
				t.Fatalf("worker upload without a fence = %d %s", response.Code, response.Body.String())
			}
			hubfixture.RequireCode(t, upload(1, lease.WorkItemID, "wrong-item", true), http.StatusConflict, "stale_fencing_token")
			wantPuts := 2
			if tc.atReserve {
				wantPuts = 1
			}
			if puts != wantPuts {
				t.Errorf("object PUTs = %d, want %d", puts, wantPuts)
			}
			if got := usage(); got != before {
				t.Errorf("rejected uploads changed quota usage from %d to %d", before, got)
			}
			var links int
			if err := tenant.Database.QueryRowContext(t.Context(), `SELECT count(*) FROM attachment_archive_references WHERE organization_id=$1 AND project_id='prj_runner'`, org).Scan(&links); err != nil || links != 1 {
				t.Errorf("archive links = %d, error = %v", links, err)
			}
			store.Mu.Lock()
			defer store.Mu.Unlock()
			if _, ok := store.Objects[retained.Key]; !ok || len(store.Objects) != 1 {
				t.Errorf("accepted archive present = %t, stored objects = %d", ok, len(store.Objects))
			}
		})
	}
}
