package nativechange

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/diagnostics"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativePostDiffFailureClearsOperationPending(t *testing.T) {
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Diff diagnostics")
	h.claim(t, issue.ID)
	e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	e.SetDiffSource(nativeChangeDiff(nativeChangeFixtureHead("c"), "README.md"))
	transport := fixtureClient(h.native.Transport()).requests.Load()
	fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/diff") {
			return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"unavailable","message":"Hub unavailable"}`)), Request: request}, nil
		}
		return transport.RoundTrip(request)
	}))
	t.Cleanup(func() { fixtureClient(h.native.Transport()).requests.Store(transport) })
	for _, nested := range []bool{false, true} {
		var done func(error)
		if nested {
			done = e.diagnosticOperation("native.finish")
		}
		var failure *APIError
		err := e.postDiff(t.Context(), 1)
		if !errors.As(err, &failure) || failure.Status != http.StatusBadGateway {
			t.Fatalf("postDiff failure = %v, want Hub 502", err)
		}
		if nested {
			done(err)
		}
		records := fixtureDiagnostics(h.scheduler, e.claim.source)
		if len(records) != 1 || records[0].OperationPending || records[0].LatestError == "" {
			t.Fatalf("nested=%t failed operation remained pending: %+v", nested, records)
		}
	}
}

func TestNativeDiagnosticOwner(t *testing.T) {
	now := time.Date(2026, 10, 9, 13, 20, 0, 0, time.UTC)
	source := &NativeConnector{}
	s := &executionAuthority{now: func() time.Time { return now }, leaseTTL: time.Minute}
	claims := map[string]executioncontract.ExecutionClaim{}
	readClaims := func(source *NativeConnector) []runnerauth.DiagnosticAttempt {
		views := make([]executioncontract.ExecutionClaim, 0, len(claims))
		for _, view := range claims {
			views = append(views, view)
		}
		return diagnostics.Claims(views, source, now, time.Minute)
	}
	c := nativeClaim{source: source, lease: tracker.NativeLease{ID: "lease_test", WorkItemID: "wi_completed", FencingToken: 22, RenewedAt: now}}
	e := &nativeExecution{scheduler: s, claim: c, data: tracker.NativeRunData{AttemptID: "attempt_test", Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: 4, Generation: 9, Phase: "completed"}}}
	c.execution = e
	claims["wi_completed"] = executioncontract.ExecutionClaim{Source: c.source, Lease: c.lease, Execution: e}
	e.ProviderCompleted(now.Add(-time.Minute))
	e.HostOperation("workspace.finalize_native_work", now.Add(-time.Second), nil, true)
	e.HostOperation("workspace.finalize_native_work", now, errors.Join(context.DeadlineExceeded, errors.New("token=private customer prompt")), true)
	for _, busy := range []bool{false, true} {
		if busy {
			e.mu.Lock()
		}
		r := readClaims(source)
		if busy {
			e.mu.Unlock()
		}
		if len(r) != 1 || r[0].ProviderCompletedAt != now.Add(-time.Minute) || r[0].HostOperation != "workspace.finalize_native_work" || !r[0].OperationPending || r[0].LatestError != runnerauth.DiagnosticError("context deadline exceeded") || r[0].LatestErrorCode != "context_deadline_exceeded" {
			t.Fatalf("busy=%t records=%+v", busy, r)
		}
	}
	for _, test := range []struct{ name, details, want string }{
		{"known", `,"details":{"rejection":{"category":"runtime_validation","field":"data.runtime.activity"}}`, "publication rejected: category=runtime_validation field=data.runtime.activity [private error text omitted]"},
		{"version receipt", `,"details":{"rejection":{"category":"version_validation","field":"validation"}}`, "publication rejected: category=version_validation field=validation [private error text omitted]"},
		{"source verification", `,"details":{"rejection":{"category":"source_verification","field":"source"}}`, "publication rejected: category=source_verification field=source [private error text omitted]"},
		{"forged version field", `,"details":{"rejection":{"category":"version_validation","field":"sentinel-secret"}}`, "[private error text omitted]"},
		{"legacy", "", "[private error text omitted]"},
		{"unknown", `,"details":{"rejection":{"category":"secret","field":"data.runtime.activity"}}`, "[private error text omitted]"},
		{"forged field", `,"details":{"rejection":{"category":"runtime_validation","field":"secret"}}`, "[private error text omitted]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var failure APIError
			if err := json.Unmarshal([]byte(`{"code":"invalid_request","message":"sentinel-secret"`+test.details+`}`), &failure); err != nil {
				t.Fatal(err)
			}
			for _, host := range []bool{false, true} {
				if host {
					e.HostOperation("native.publishVersion", now, &failure, true)
				} else {
					e.diagnosticOperation("append.run.observed")(&failure)
				}
				records := readClaims(source)
				if len(records) != 1 || records[0].LatestError != test.want || records[0].LatestErrorCode != "invalid_request" || records[0].NativeAttemptID != "attempt_test" || !records[0].OperationObservedAt.Equal(now) || !records[0].OperationPending {
					t.Fatalf("lost rejection or correlation: %+v", records)
				}
			}
		})
	}

	if len(readClaims(&NativeConnector{})) != 0 {
		t.Fatal("cross-project claim disclosed")
	}
	e.pending = &tracker.NativeRunEvent{Type: "run.finished"}
	pending := e.pending
	r := readClaims(source)
	if r[0].HostOperation != "append.run.finished" || e.pending != pending || e.data.Sequence != 0 {
		t.Fatal("pending completion advanced")
	}
	if len(claims) != 1 || claims["wi_completed"].Lease.RenewedAt != now {
		t.Fatal("claim changed")
	}
	for _, test := range []struct {
		name     string
		lost     bool
		terminal bool
		expires  time.Time
		deadline time.Time
		want     int
	}{
		{name: "lost claim", lost: true},
		{name: "terminal observation", terminal: true},
		{name: "expiry without monotonic deadline", expires: now.Add(-time.Minute - time.Nanosecond)},
		{name: "server clock behind runner", expires: now.Add(-time.Hour), deadline: now.Add(time.Minute), want: 1},
		{name: "server clock ahead of runner", expires: now.Add(time.Hour), deadline: now.Add(-time.Minute - time.Nanosecond)},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim := claims["wi_completed"]
			claim.Lost, claim.Recovery.Issue.Terminal = test.lost, test.terminal
			claim.Lease.ExpiresAt, claim.Deadline = test.expires, test.deadline
			claims["wi_completed"] = claim
			defer func() {
				claims["wi_completed"] = executioncontract.ExecutionClaim{Source: c.source, Lease: c.lease, Execution: e}
			}()
			if records := readClaims(source); len(records) != test.want {
				t.Fatalf("native claim diagnostics = %+v, want %d records", records, test.want)
			}
			if claims["wi_completed"].Execution != e || e.pending != pending || e.data.Sequence != 0 {
				t.Fatal("claim diagnostics discarded or advanced completion custody")
			}
		})
	}
}
