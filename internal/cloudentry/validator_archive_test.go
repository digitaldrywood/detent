package cloudentry

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestValidatorArchiveUploadChecksLeaseAtPublication(t *testing.T) {
	for _, tc := range []struct {
		name                                                  string
		revoke, atReserve, deleteFails, rotation, lostRefusal bool
		failedPut, lostPublication                            bool
	}{
		{name: "expires before quota reservation", atReserve: true},
		{name: "expires during object upload"},
		{name: "revoked during object upload", revoke: true},
		{name: "failed object deletion keeps quota charged", deleteFails: true},
		{name: "lost publication refusal keeps cleanup pending", lostRefusal: true},
		{name: "abandoned reservation before object storage", failedPut: true},
		{name: "abandoned reservation after object storage", lostPublication: true},
		{name: "expired replay after binding rotation preserves published archive", rotation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			abandoned := tc.failedPut || tc.lostPublication
			var advance atomic.Int64
			var clockStart atomic.Pointer[time.Time]
			now := func() time.Time {
				if start := clockStart.Load(); start != nil {
					return start.Add(time.Duration(advance.Load()))
				}
				return time.Now()
			}
			f := newEntryFixtureOn(t, nil, slog.New(slog.DiscardHandler), nil, func(cfg *hubserver.Config) {
				cfg.Clock = now
			})
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
			failDelete := tc.deleteFails
			transport := runtimeTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodDelete && failDelete && strings.Contains(req.URL.Path, "/blobs/sha256/") {
					response := httptest.NewRecorder()
					response.WriteHeader(http.StatusForbidden)
					return response.Result(), nil
				}
				if req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/blobs/sha256/") {
					puts++
					if armed && tc.failedPut {
						response := httptest.NewRecorder()
						response.WriteHeader(http.StatusForbidden)
						return response.Result(), nil
					}
					if armed && !ended && !tc.atReserve && !tc.rotation && !abandoned {
						if err := endLease(); err != nil {
							return nil, err
						}
					}
				}
				return store.Transport.RoundTrip(req)
			})
			if tc.atReserve || tc.rotation || tc.lostRefusal || tc.lostPublication {
				original := f.service.config.Transport
				checks := 0
				path := "/api/v2/organizations/" + tenant.ID + "/projects/prj_runner/attachment-metadata/archives/" + string(lease.WorkItemID)
				f.service.config.Transport = func(organization Organization) (http.RoundTripper, error) {
					next, err := original(organization)
					return runtimeTransport(func(req *http.Request) (*http.Response, error) {
						if armed && tc.lostPublication && req.Method == http.MethodPost && req.URL.Path == path {
							return nil, io.ErrUnexpectedEOF
						}
						if armed && !ended && req.Method == http.MethodPost {
							if tc.atReserve && req.URL.Path == path+"/check" {
								checks++
							}
							if tc.rotation && req.URL.Path == path || tc.atReserve && checks == 2 {
								if err := endLease(); err != nil {
									return nil, err
								}
							}
						}
						response, err := next.RoundTrip(req)
						if tc.lostRefusal && armed && req.Method == http.MethodPost && req.URL.Path == path && err == nil && response.StatusCode == http.StatusConflict {
							_ = response.Body.Close()
							return nil, io.ErrUnexpectedEOF
						}
						return response, err
					}), err
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
			start := time.Now()
			clockStart.Store(&start)
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
			published := []string{retained.Key}
			if tc.rotation {
				first := upload(0, lease.WorkItemID, "interrupted", true)
				hubfixture.RequireStatus(t, first, http.StatusCreated)
				var ref attachment.ArchiveReference
				if err := json.Unmarshal(first.Body.Bytes(), &ref); err != nil {
					t.Fatal(err)
				}
				published = append(published, ref.Key)
				binding, err := f.service.currentArchiveBinding(t.Context(), tenant.ID)
				if err != nil {
					t.Fatal(err)
				}
				next, err := f.service.saveArchiveBinding(t.Context(), binding, binding.Revision)
				if err != nil || next.ID == binding.ID {
					t.Fatalf("rotate hosted binding: %v", err)
				}
			}
			before := usage()
			armed = true
			rejected := upload(0, lease.WorkItemID, "interrupted", true)
			if ended == abandoned {
				t.Fatalf("unexpected lease state after interrupted upload: ended=%t, status=%d %s", ended, rejected.Code, rejected.Body.String())
			}
			if tc.lostRefusal || abandoned {
				hubfixture.RequireStatus(t, rejected, http.StatusBadGateway)
			} else {
				hubfixture.RequireCode(t, rejected, http.StatusConflict, "stale_fencing_token")
			}
			if !abandoned {
				hubfixture.RequireCode(t, upload(0, lease.WorkItemID, "retry", true), http.StatusConflict, "stale_fencing_token")
			}
			if response := upload(0, lease.WorkItemID, "missing-fence", false); response.Code < 400 || response.Code >= 500 {
				t.Fatalf("worker upload without a fence = %d %s", response.Code, response.Body.String())
			}
			hubfixture.RequireCode(t, upload(1, lease.WorkItemID, "wrong-item", true), http.StatusConflict, "stale_fencing_token")
			wantPuts := 2
			if tc.atReserve {
				wantPuts = 1
			}
			if tc.rotation {
				wantPuts = 3
			}
			if puts != wantPuts {
				t.Errorf("object PUTs = %d, want %d", puts, wantPuts)
			}
			if got := usage(); tc.atReserve && got != before || !tc.atReserve && got <= before {
				t.Errorf("quota before object deletion = %d, initial quota = %d", got, before)
			}
			organization, err := f.service.readyOrganization(t.Context(), tenant.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.sweepOrganizationAttachments(t.Context(), organization); err != nil {
				t.Fatal(err)
			}
			charged := usage()
			if !tc.atReserve && charged <= before {
				t.Errorf("quota refunded before pending writes drained: %d", charged)
			}
			if tc.lostPublication {
				advance.Store(int64(45 * time.Second))
				hubfixture.RequireStatus(t, upload(0, lease.WorkItemID, "interrupted", true), http.StatusBadGateway)
				if got := usage(); puts != wantPuts || got != charged {
					t.Errorf("retry did not reuse its reserved object and charge: puts=%d, quota=%d, initial quota=%d", puts, got, before)
				}
				advance.Store(int64(75 * time.Second))
				if err := f.service.sweepOrganizationAttachments(t.Context(), organization); err != nil {
					t.Fatal(err)
				}
				if got := usage(); got <= before {
					t.Errorf("sweep refunded a refreshed reservation: %d", got)
				}
			}
			advance.Store(int64(2 * time.Minute))
			if tc.lostPublication {
				advance.Store(int64(105 * time.Second))
			}
			if tc.failedPut {
				advance.Store(int64(time.Minute))
			}
			if tc.deleteFails {
				if err := f.service.sweepOrganizationAttachments(t.Context(), organization); err == nil {
					t.Error("deletion failure was not reported")
				}
				if got := usage(); got <= before {
					t.Errorf("failed deletion refunded quota to %d", got)
				}
				failDelete = false
			}
			for range 2 {
				if err := f.service.sweepOrganizationAttachments(t.Context(), organization); err != nil {
					t.Fatal(err)
				}
				if got := usage(); got != before {
					t.Errorf("quota after object deletion = %d, want %d", got, before)
				}
			}
			if abandoned {
				hubfixture.RequireStatus(t, upload(0, lease.WorkItemID, "interrupted", true), http.StatusConflict)
				if puts != wantPuts || usage() != before {
					t.Error("a cleaned reservation regained storage or quota")
				}
			}
			var links int
			if err := tenant.Database.QueryRowContext(t.Context(), `SELECT count(*) FROM attachment_archive_references WHERE organization_id=$1 AND project_id='prj_runner'`, org).Scan(&links); err != nil || links != len(published) {
				t.Errorf("archive links = %d, error = %v", links, err)
			}
			store.Mu.Lock()
			defer store.Mu.Unlock()
			for _, key := range published {
				if _, ok := store.Objects[key]; !ok {
					t.Error("a published archive was deleted")
				}
			}
			if len(store.Objects) != len(published) {
				t.Errorf("stored objects = %d, want %d", len(store.Objects), len(published))
			}
		})
	}
}
