package runnerupdate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type receiptFixture struct {
	service     *hubtest.HTTPHarness
	bindings    []runnerauth.Binding
	tokens      []string
	credentials []hubtest.SeededCredential
}

func newReceiptFixture(t *testing.T) receiptFixture {
	t.Helper()
	repository := hubtest.Repository(t)
	organization := hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: "org_startup"}).OrganizationID
	project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{})
	h := hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repository, Organization: organization}, func(group *echo.Group, deps hubtest.HTTPDeps) {
		service := New(Deps{Repository: repository, Now: time.Now})
		RegisterRoutes(group, HTTPDeps{Service: func() *Service { return service }, Auth: func() *hubauth.Authenticator { return deps.Auth }, Admin: deps.API(hubauth.Admin), Worker: deps.API(hubauth.Worker), Error: deps.Error, Invalid: func(c echo.Context, err error) error { return deps.Error(c, hubapi.Invalid(err.Error())) }})
	})
	f := receiptFixture{service: h}
	for range 2 {
		actor, runner := h.RunnerCredential(t, hubtest.RunnerOptions{Projects: []tracker.ProjectID{project.ID}})
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.ReplaceRunnerBackendIsolation(t.Context(), runner.Record.RunnerID, isolation.Report{"codex": {isolation.NativeTrusted, isolation.Sandbox}})
		}); err != nil {
			t.Fatal(err)
		}
		f.bindings = append(f.bindings, runner.Identity.Binding)
		f.tokens = append(f.tokens, actor.Token)
		f.credentials = append(f.credentials, runner.Credential)
	}
	return f
}

func performHubAPIRequest(t *testing.T, h *hubtest.HTTPHarness, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return h.Request(t, hubtest.HTTPActor{Token: token}, method, path, body).Recorder
}
func requireNativeStatus(t *testing.T, r *httptest.ResponseRecorder, want int) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("status = %d, want %d: %s", r.Code, want, r.Body.String())
	}
}
func writeMonotonicReceipt(t *testing.T, f receiptFixture, index int, writer string, receipt runnerauth.UpdateReceipt, build runnerauth.BuildEvidence, allowed ...int) {
	t.Helper()
	path := "/api/v2/organizations/org_startup/runners/" + f.bindings[index].RunnerID + "/update/receipt"
	response := performHubAPIRequest(t, f.service, http.MethodPut, path, f.tokens[index], ReceiptWrite{Receipt: receipt, Running: build})
	if response.Code != http.StatusNoContent && !slices.Contains(allowed, response.Code) {
		t.Errorf("%s: status %d: %s", writer, response.Code, response.Body.String())
	}
}
func assertMonotonicReceipt(t *testing.T, f receiptFixture, index int, want runnerauth.UpdateReceipt) {
	t.Helper()
	path := "/api/v2/organizations/org_startup/runners/" + f.bindings[index].RunnerID + "/update/receipt"
	response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.tokens[index], nil)
	requireNativeStatus(t, response, http.StatusOK)
	var got runnerauth.UpdateReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("durable receipt regressed: got %+v want %+v", got, want)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.tokens[1-index], nil), http.StatusUnauthorized)
}

func TestPostgresRunnerUpdateReceiptMonotonic(t *testing.T) {
	for _, mode := range []string{"put-put"} {
		t.Run(mode, func(t *testing.T) {
			f := newReceiptFixture(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			for index := range f.bindings {
				request := runnerauth.UpdateRequest{ID: fmt.Sprintf("update_%d", index), RequestedAt: now, Service: "detent", Version: "1.1.0", Release: true, ExpectedBuildRevision: strings.Repeat("c", 64)}
				seedMonotonicRequest(t, f, index, request)
				build := runnerauth.BuildEvidence{Version: "1.1.0", Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Source: "release", VerifiedRelease: true, OS: "linux", Architecture: "amd64", ObservedAt: now}
				old := runnerauth.UpdateReceipt{Request: request, Status: "draining", ObservedAt: now}
				newer := runnerauth.UpdateReceipt{Request: request, Status: "applied", Applied: &build, ObservedAt: now.Add(time.Second)}
				if index == 1 {
					newer.ObservedAt = old.ObservedAt
				}
				writers := strings.Split(mode, "-")

				if index == 1 {
					writeMonotonicReceipt(t, f, index, writers[1], newer, build)
				}
				writeMonotonicReceipt(t, f, index, writers[0], old, build)
				writeMonotonicReceipt(t, f, index, writers[1], newer, build)
				writeMonotonicReceipt(t, f, index, writers[0], old, build)
				assertMonotonicReceipt(t, f, index, newer)
				writeMonotonicReceipt(t, f, index, "put", newer, build)
				assertMonotonicReceipt(t, f, index, newer)
			}
		})
	}
}

func TestPostgresRunnerUpdateRefusalPreservesPendingTarget(t *testing.T) {
	f := newReceiptFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for index, name := range []string{"installed", "detached"} {
		t.Run(name, func(t *testing.T) {
			newer := runnerauth.UpdateRequest{ID: fmt.Sprintf("newer_%d", index), RequestedAt: now, Service: "detent", Version: "1.2.4", Release: true, FollowHub: true, ExpectedBuildRevision: strings.Repeat("c", 64)}
			seedMonotonicRequest(t, f, index, newer)
			running := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Source: "release", VerifiedRelease: true, OS: "linux", Architecture: "amd64", ObservedAt: now}
			target := running
			target.Version, target.SHA256 = newer.Version, strings.Repeat("d", 64)
			applied := runnerauth.UpdateReceipt{Request: newer, Status: "applied", Applied: &target, ObservedAt: now}
			if name == "detached" {
				applied.Status, applied.Applied, applied.VerifiedTarget = "uncertain", nil, &target
			}
			writeMonotonicReceipt(t, f, index, "install", applied, running)
			older := newer
			older.ID, older.Version, older.RequestedAt = fmt.Sprintf("older_%d", index), running.Version, now.Add(time.Second)
			seedMonotonicRequest(t, f, index, older)
			refusal := runnerauth.UpdateReceipt{Request: older, Status: "refused", FailureReason: "A newer release is awaiting restart", ObservedAt: older.RequestedAt}
			writeMonotonicReceipt(t, f, index, "refuse", refusal, running)
			want := applied
			want.PreviousFailure = &runnerauth.UpdateFailure{Request: older, FailureReason: refusal.FailureReason, ObservedAt: refusal.ObservedAt}
			assertMonotonicReceipt(t, f, index, want)
			writeMonotonicReceipt(t, f, index, "repeat install", applied, running)
			assertMonotonicReceipt(t, f, index, want)
			runner := runnerauth.Runner{Binding: f.bindings[index], Health: "online", ConnectionHealth: "online", LastHeartbeatAt: now,
				Routing: runnerauth.Routing{UpdateRequest: &older}, Update: &runnerauth.UpdateObservation{Supported: true, Running: running, Receipt: &want, ReceivedAt: now}}
			if view := runner.UpdateView(now); view.Status != "refused" {
				t.Fatalf("desired request status = %q, want refused", view.Status)
			}
		})
	}
}

func seedMonotonicRequest(t *testing.T, f receiptFixture, index int, request runnerauth.UpdateRequest) {
	t.Helper()
	err := f.service.Repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		id := f.bindings[index].RunnerID
		identity, err := w.RunnerIdentity(t.Context(), id)
		if err != nil {
			return err
		}
		identity.DisplayName = "Example"
		if err := w.UpdateRunnerIdentity(t.Context(), identity); err != nil {
			return err
		}
		owner := id
		if request.Urgent {
			owner = ""
		}
		if err := w.CreateRunnerUpdateRequest(t.Context(), runnerstore.UpdateRequestRecord{OrganizationID: "org_startup", RunnerID: owner, Request: request}); err != nil {
			return err
		}
		if request.Urgent {
			urgent, err := w.RunnerUrgentUpdate(t.Context())
			if err != nil {
				return err
			}
			return w.SetRunnerUrgentUpdate(t.Context(), urgent.Revision, request.ID, request.RequestedAt)
		}
		identity, err = w.RunnerIdentity(t.Context(), id)
		if err != nil {
			return err
		}
		return w.SetRunnerUpdateDesired(t.Context(), id, identity.Revision, request.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPostgresRunnerUpdateReceiptRequiresCurrentCredential(t *testing.T) {
	f := newReceiptFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for index := range f.bindings {
		request := runnerauth.UpdateRequest{ID: fmt.Sprintf("credential_%d", index), RequestedAt: now, Service: "detent", Version: "1.1.0", Release: true, ExpectedBuildRevision: strings.Repeat("c", 64)}
		seedMonotonicRequest(t, f, index, request)
		build := runnerauth.BuildEvidence{Version: "1.0.0", Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Source: "release", VerifiedRelease: true, OS: "linux", Architecture: "amd64", ObservedAt: now}
		receipt := runnerauth.UpdateReceipt{Request: request, Status: "draining", ObservedAt: now}
		path := "/api/v2/organizations/org_startup/runners/" + f.bindings[index].RunnerID + "/update/receipt"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, path, f.tokens[1-index], ReceiptWrite{Receipt: receipt, Running: build}), http.StatusUnauthorized)
		writeMonotonicReceipt(t, f, index, "put", receipt, build)
		if err := f.service.Repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			return w.RevokeAccessCredential(t.Context(), f.credentials[index].ID, now)
		}); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, path, f.tokens[index], ReceiptWrite{Receipt: receipt, Running: build}), http.StatusUnauthorized)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.tokens[index], nil), http.StatusUnauthorized)
	}
}
