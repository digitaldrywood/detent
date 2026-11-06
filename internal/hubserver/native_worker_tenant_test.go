package hubserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type leaseTransactionKey struct{}

type leaseTransactionCounter struct {
	tenantstore.Repository
	reads, writes int
}

func (r *leaseTransactionCounter) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	if ctx.Value(leaseTransactionKey{}) == true {
		r.reads++
	}
	return r.Repository.ReadTenant(ctx, org, workload, read)
}
func (r *leaseTransactionCounter) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	if ctx.Value(leaseTransactionKey{}) == true {
		r.writes++
	}
	return r.Repository.WriteTenant(ctx, org, workload, write)
}

func TestLeaseRenewalReturnsItsCommittedSnapshot(t *testing.T) {
	f := newRunnerPGFixture(t)
	credential, status, err := f.service.authenticateAccessHash(t.Context(), apikey.HashToken(f.tokens[0]), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("authentication: status=%d err=%v", status, err)
	}
	service := &Service{config: f.service.config}
	service.config.NativeRunEvents = tenantNativeRunEventParticipant{service: service}
	counter := &leaseTransactionCounter{Repository: service.config.TenantRepository}
	service.config.TenantRepository = counter
	response := httptest.NewRecorder()
	request := newHubAPIRequest(t, http.MethodPost, "/renew", f.tokens[0], tracker.NativeLeaseMutation{FencingToken: f.leases[0].FencingToken, TTLSeconds: 300})
	var statements []string
	ctx := postgres.WithTransactionTiming(context.WithValue(t.Context(), leaseTransactionKey{}, true), nil, func(_ time.Time, query string) { statements = append(statements, query) })
	request = request.WithContext(ctx)
	c := echo.New().NewContext(request, response)
	c.SetParamNames("lease")
	c.SetParamValues(string(f.leases[0].ID))
	c.Set("native_scope", nativeScope{organization: "org_startup", project: "prj_runner", credential: credential})
	if err := service.renewNativeLeaseTenant(c); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, response, http.StatusOK)
	var renewed tracker.NativeLease
	decodeHubResponse(t, response, &renewed)
	if renewed.ID != f.leases[0].ID || renewed.FencingToken != f.leases[0].FencingToken || renewed.PolicyID != f.policy.PolicyID || renewed.IsolationPolicy == nil {
		t.Fatalf("renewal lost authority or response details: %+v", renewed)
	}
	if got := len(statements) + 1; got > 8 {
		t.Errorf("renew command exchanges=%d, want at most 8 including BEGIN/binding", got)
	}
	t.Logf("renew command statements excluding BEGIN/binding=%d", len(statements))
	for _, query := range statements {
		if len(query) > 100 {
			query = query[:100]
		}
		t.Logf("SQL: %s", query)
	}
	if counter.reads != 0 || counter.writes != 1 {
		t.Fatalf("renewal transactions read=%d write=%d, want 0 and 1", counter.reads, counter.writes)
	}
}

func TestLeaseCommandLocksRunnerBeforeFence(t *testing.T) {
	for _, operation := range []string{"renew", "release", "expire"} {
		t.Run(operation, func(t *testing.T) {
			f := newRunnerPGFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			blocker, err := f.admin.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			if _, err := blocker.ExecContext(ctx, `SELECT runner_id FROM runner_identities WHERE organization_id='org_startup' AND runner_id=$1 FOR UPDATE`, f.bindings[0].RunnerID); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				result <- f.service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Background, func(w tenantstore.Writer) error {
					fence := leasestore.Fence{LeaseID: string(f.leases[0].ID), FencingToken: int64(f.leases[0].FencingToken)}
					switch operation {
					case "renew":
						_, err := w.Renew(ctx, fence, time.Hour)
						return err
					case "release":
						_, err := w.Release(ctx, fence, "released")
						return err
					default:
						_, err := w.Expire(ctx, fence)
						return err
					}
				})
			}()
			t.Cleanup(func() { cancel(); _ = blocker.Rollback(); <-done })
			testenv.WaitUntil(t, ctx, func() error {
				select {
				case err := <-result:
					t.Fatalf("command finished before runner lock: %v", err)
				default:
				}
				var waiting bool
				if err := f.admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%MutateLeaseCommand%')`).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("lease command has not reached runner lock")
				}
				return nil
			})
			if _, err := blocker.ExecContext(ctx, `SELECT lease_id FROM leases WHERE organization_id='org_startup' AND lease_id=$1 FOR UPDATE NOWAIT`, string(f.leases[0].ID)); err != nil {
				t.Fatalf("lease was locked before its runner: %v", err)
			}
			if err := blocker.Rollback(); err != nil {
				t.Fatal(err)
			}
			err = <-result
			if operation == "expire" {
				if !errors.Is(err, tracker.ErrStaleFencingToken) {
					t.Fatalf("expire live lease error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
