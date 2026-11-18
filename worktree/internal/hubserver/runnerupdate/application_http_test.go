package runnerupdate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type updateHTTPFixture struct {
	h        *hubtest.HTTPHarness
	service  *Service
	database *sql.DB
	details  *hubtest.Details
	clock    *hubtest.Clock
	admin    hubtest.HTTPActor
	actor    hubtest.HTTPActor
	runner   hubtest.SeededRunner
	path     string
}

func newUpdateHTTPFixture(t *testing.T, clock *hubtest.Clock) updateHTTPFixture {
	t.Helper()
	f := updateHTTPFixture{database: hubtest.Database(t), details: hubtest.NewDetails(), clock: clock}
	repository := hubtest.RepositoryOn(t, f.database, hubtest.WithClock(clock.Now))
	f.h = hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repository, Now: clock.Now}, func(group *echo.Group, deps hubtest.HTTPDeps) {
		f.service = New(Deps{Repository: repository, Now: clock.Now, Details: func() attachment.DetailStore { return f.details }, ApplyUsage: func(context.Context, billingstore.Writer, time.Time, string, bool, bool) error { return nil }})
		RegisterRoutes(group, HTTPDeps{Service: func() *Service { return f.service }, Auth: func() *hubauth.Authenticator { return deps.Auth }, Admin: deps.API(hubauth.Admin), Worker: deps.API(hubauth.Worker), Error: deps.Error, Invalid: func(c echo.Context, err error) error { return deps.Error(c, hubapi.Invalid(err.Error())) }})
	})
	project := hubtest.Project(t, repository, f.h.Organization, hubtest.ProjectOptions{})
	f.admin = f.h.AdminKey(t)
	f.actor, f.runner = f.h.RunnerCredential(t, hubtest.RunnerOptions{Projects: []tracker.ProjectID{project.ID}})
	f.path = "/api/v2/organizations/" + string(f.h.Organization) + "/runners/" + f.runner.Identity.RunnerID + "/update"
	return f
}

func (f updateHTTPFixture) observe(t *testing.T, report *runnerauth.UpdateObservation) {
	t.Helper()
	if err := f.h.Repository.WriteTenant(t.Context(), f.h.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		identity, err := w.RunnerIdentity(t.Context(), f.runner.Identity.RunnerID)
		if err != nil {
			return err
		}
		return f.service.StoreObservation(t.Context(), w, identity, report, 2, report.Running.Version, report.Running.OS, report.Running.Architecture, f.clock.Now(), false)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerUpdateObservationOwner(t *testing.T) {
	for _, test := range []struct {
		name    string
		old     bool
		foreign bool
	}{
		{"old protocol removes update owner", true, false},
		{"foreign receipt preserves build observation", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Microsecond)
			f := newUpdateHTTPFixture(t, hubtest.NewClock(now))
			build := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
			report := &runnerauth.UpdateObservation{Discovery: "unknown", Protocol: 1, Service: "detent", Supported: true, Running: build, ObservedAt: now}
			report.Revision = report.BuildRevision()
			if test.old {
				f.observe(t, report)
			}
			if test.foreign {
				request := runnerauth.UpdateRequest{ID: "update_unknown", RequestedAt: now, Service: "detent", Version: "1.2.4", Release: true, Urgent: true}
				report.Receipt = &runnerauth.UpdateReceipt{Request: request, Status: "refused", FailureReason: "Update was not applied", ObservedAt: now}
			} else {
				report = nil
			}
			if err := f.h.Repository.WriteTenant(t.Context(), f.h.Organization, corestore.Serving, func(w tenantstore.Writer) error {
				identity, err := w.RunnerIdentity(t.Context(), f.runner.Identity.RunnerID)
				if err != nil {
					return err
				}
				protocol := 2
				if test.old {
					protocol = 1
				}
				return f.service.StoreObservation(t.Context(), w, identity, report, protocol, build.Version, build.OS, build.Architecture, now, false)
			}); err != nil {
				t.Fatal(err)
			}
			response := performHubAPIRequest(t, f.h, http.MethodGet, f.path, f.admin.Token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var view runnerauth.UpdateView
			decodeHubResponse(t, response, &view)
			if test.old && (view.Status != "unavailable" || view.Observation != nil) {
				t.Fatalf("older runner owner=%+v", view)
			}
			if test.foreign && (view.Observation == nil || view.Observation.Receipt != nil || view.Observation.Running != build) {
				t.Fatalf("foreign receipt replaced build observation: %+v", view)
			}
		})
	}
}

func decodeHubResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatal(err)
	}
}

func requireNativeError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) hubapi.Error {
	t.Helper()
	requireNativeStatus(t, response, status)
	var failure hubapi.Error
	decodeHubResponse(t, response, &failure)
	if failure.Code != code {
		t.Fatalf("error code = %q, want %q: %s", failure.Code, code, response.Body.String())
	}
	return failure
}

func (f updateHTTPFixture) requireCommandObject(t *testing.T, operation, key string, want []byte) {
	t.Helper()
	var receipt eventstore.Receipt
	if err := f.h.Repository.ReadTenant(t.Context(), f.h.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		receipt, err = r.Receipt(t.Context(), f.admin.Credential.ID, operation, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if receipt.PayloadObjectKey == "" {
		t.Fatalf("%s has no immutable response object", operation)
	}
	raw, err := eventstore.NewContent(f.details).Receipt(t.Context(), receipt)
	if err != nil || !bytes.Equal(bytes.TrimSpace(raw), bytes.TrimSpace(want)) {
		t.Fatalf("%s object lost response: %v", operation, err)
	}
}

func TestRunnerUpdateApplication(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := newUpdateHTTPFixture(t, hubtest.NewClock(now))
	build := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), Source: "private_patched_source", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
	report := &runnerauth.UpdateObservation{Discovery: "available", Protocol: 1, Service: "detent", Supported: true, AvailableVersion: "1.2.4", AvailableObservedAt: now, Running: build, ObservedAt: now}
	report.Revision = report.BuildRevision()
	f.observe(t, report)
	read := func() runnerauth.UpdateView {
		var view runnerauth.UpdateView
		response := performHubAPIRequest(t, f.h, http.MethodGet, f.path, f.admin.Token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &view)
		return view
	}
	view := read()
	if view.Status != "ready" || view.Observation.Running.Source != "private_patched_source" || view.Observation.Running.VerifiedRelease {
		t.Fatalf("initial view=%+v", view)
	}
	for _, test := range []struct {
		name                    string
		staleRunner, staleBuild bool
		prefixed                bool
	}{
		{name: "current same-version repair"},
		{name: "prefixed same-version repair", prefixed: true},
		{name: "prefixed stale runner revision", prefixed: true, staleRunner: true},
		{name: "prefixed stale build revision", prefixed: true, staleBuild: true},
		{name: "stale runner revision", staleRunner: true},
		{name: "stale build revision", staleBuild: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var repairRunner runnerauth.Runner
			err := f.h.Repository.ReadTenant(t.Context(), f.h.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
				var err error
				repairRunner, err = f.service.runner(t.Context(), reader, f.runner.Identity.RunnerID, now)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			repair := *repairRunner.Update
			repair.Running.Source = "release"
			if test.prefixed {
				repair.Running.Version = "v" + repair.Running.Version
			}
			repair.AvailableVersion = ""
			repair.Discovery = "up_to_date"
			repair.Revision = repair.BuildRevision()
			repairRunner.Update = &repair
			request := Change{ExpectedRevision: repairRunner.Revision, ExpectedBuildRevision: repair.Revision, Service: "detent", Version: strings.TrimPrefix(repair.Running.Version, "v"), Release: true}
			if test.staleRunner {
				request.ExpectedRevision++
			}
			if test.staleBuild {
				request.ExpectedBuildRevision = strings.Repeat("e", 64)
			}
			err = Ready(repairRunner, request, f.clock.Now())
			if (err != nil) != (test.staleRunner || test.staleBuild) {
				t.Fatalf("same-version repair selection: %v", err)
			}
		})
	}
	request := Change{ExpectedRevision: view.Revision, ExpectedBuildRevision: report.Revision, Service: "detent", Version: "1.2.4", Release: true, Confirm: true, IdempotencyKey: "selected-release"}
	staleBuild := request
	staleBuild.ExpectedBuildRevision = strings.Repeat("e", 64)
	failure := requireNativeError(t, performHubAPIRequest(t, f.h, http.MethodPost, f.path+"/apply", f.admin.Token, staleBuild), http.StatusConflict, "revision_conflict")
	if failure.CurrentRevision != tracker.Revision(view.Revision) || !strings.Contains(failure.Message, "build revision") {
		t.Fatalf("stale build refusal=%+v", failure)
	}
	unavailable := *report
	unavailable.Supported = false
	unavailable.Revision = unavailable.BuildRevision()
	f.observe(t, &unavailable)
	if got := read(); got.Status != "unavailable" || got.Observation == nil || got.Observation.Protocol != 1 {
		t.Fatalf("missing installed owner=%+v", got)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.h, http.MethodPost, f.path+"/apply", f.admin.Token, request), http.StatusServiceUnavailable)
	f.observe(t, report)
	noConfirm := request
	noConfirm.Confirm = false
	requireNativeStatus(t, performHubAPIRequest(t, f.h, http.MethodPost, f.path+"/apply", f.admin.Token, noConfirm), http.StatusPreconditionRequired)
	response := performHubAPIRequest(t, f.h, http.MethodPost, f.path+"/apply", f.admin.Token, request)
	requireNativeStatus(t, response, http.StatusAccepted)
	f.requireCommandObject(t, "runner_update "+f.runner.Identity.RunnerID, request.IdempotencyKey, response.Body.Bytes())
	replay := performHubAPIRequest(t, f.h, http.MethodPost, f.path+"/apply", f.admin.Token, request)
	requireNativeStatus(t, replay, http.StatusAccepted)
	if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("changed idempotent receipt")
	}
	view = read()
	if view.Status != "requested" || view.Desired == nil || view.Observation.Receipt != nil {
		t.Fatalf("queued receipt=%+v", view)
	}
	for _, test := range []struct {
		name       string
		change     func(*Change)
		credential string
		path       string
		want       int
	}{
		{"changed retry", func(c *Change) { c.Version = "1.2.5" }, f.admin.Token, f.path + "/apply", http.StatusConflict},
		{"stale request", func(c *Change) { c.IdempotencyKey = "stale" }, f.admin.Token, f.path + "/apply", http.StatusConflict},
		{"worker denied", func(c *Change) {}, f.actor.Token, f.path + "/apply", http.StatusForbidden},
		{"cross organization", func(c *Change) {}, f.admin.Token, "/api/v2/organizations/org_other/runners/" + f.runner.Identity.RunnerID + "/update/apply", http.StatusUnauthorized},
		{"shared hub service denied", func(c *Change) { c.Service = "hub" }, f.admin.Token, f.path + "/apply", http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := request
			test.change(&copy)
			response := performHubAPIRequest(t, f.h, http.MethodPost, test.path, test.credential, copy)
			if test.want == http.StatusConflict {
				code := "revision_conflict"
				if test.name == "changed retry" {
					code = "idempotency_conflict"
				}
				requireNativeError(t, response, test.want, code)
			} else {
				requireNativeStatus(t, response, test.want)
			}
		})
	}
}
