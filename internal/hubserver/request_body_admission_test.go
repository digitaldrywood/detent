package hubserver

import (
	"bytes"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

type heldRequestBody struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	reads   atomic.Int64
}

func (b *heldRequestBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	b.once.Do(func() { close(b.started) })
	<-b.release
	return 0, io.EOF
}

func (b *heldRequestBody) Close() error { return nil }

func TestLargeWorkerBodiesRefusedBeforeRead(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, suffix := range []string{"/versions", "/versions/version_example/landing"} {
			t.Run(strings.TrimPrefix(suffix, "/")+map[bool]string{false: " local", true: " shared"}[shared], func(t *testing.T) {
				f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}))
				runners := make([]runnerFixture, 2)
				for i := range runners {
					runners[i] = prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Collaborate)
					runners[i].enroll(t)
				}
				claim := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: f.issue.WorkItemID, MachineID: runners[1].binding.MachineID, SessionID: "publisher", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runners[1].redemption.Credential, claim)
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				event := nativeStartedEvent(lease)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", runners[1].redemption.Credential, event), http.StatusOK)
				input := changeTestInput()
				bundle := changeTestSource(t, &input)
				input.RunID, input.AttemptID = event.Data.RunID, event.Data.AttemptID
				key := hostedSharedKey(19)
				if shared {
					f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID), SharedEntry: &HostedSharedEntry{verifier: &cloudassert.Verifier{Issuer: "entry", Audience: string(f.project.OrganizationID), Generation: 1, PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}}}}
				}
				serve := func(request *http.Request, body []byte) *httptest.ResponseRecorder {
					recorder := httptest.NewRecorder()
					if shared {
						id, err := cloudassert.NewID()
						if err != nil {
							panic(err)
						}
						now := time.Now()
						value, err := cloudassert.Sign(key, cloudassert.Claims{Issuer: "entry", Audience: string(f.project.OrganizationID), Generation: 1, Kind: cloudassert.KindMachine, Method: request.Method, Path: request.URL.RequestURI(), BodyDigest: cloudassert.BodyDigest(body), ID: id, IssuedAt: now, ExpiresAt: now.Add(cloudassert.MaxLifetime)})
						if err != nil {
							panic(err)
						}
						request.Header.Set(cloudassert.Header, value)
						c := f.service.echo.NewContext(request, recorder)
						if err := f.service.hostedSharedEntry(func(c echo.Context) error { f.service.Handler().ServeHTTP(recorder, c.Request()); return nil })(c); err != nil {
							f.service.echo.HTTPErrorHandler(err, c)
						}
					} else {
						f.service.Handler().ServeHTTP(recorder, request)
					}
					return recorder
				}
				held := &heldRequestBody{started: make(chan struct{}), release: make(chan struct{})}
				request := newHubAPIRequest(t, http.MethodPost, f.path+suffix, runners[0].redemption.Credential, nil)
				request.Body = io.NopCloser(io.MultiReader(strings.NewReader(`{"source_bundle":"`+strings.Repeat("A", 1<<20)), held))
				request.ContentLength = maxChangeVersionRequestBytes
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() { done <- serve(request, nil) }()
				t.Cleanup(func() { close(held.release); <-done })
				<-held.started
				for range 3 {
					refusedBody := &heldRequestBody{started: make(chan struct{}), release: make(chan struct{})}
					close(refusedBody.release)
					refused := newHubAPIRequest(t, http.MethodPost, f.path+suffix, runners[0].redemption.Credential, nil)
					refused.Body = refusedBody
					refused.ContentLength = maxChangeVersionRequestBytes
					requireNativeStatus(t, serve(refused, nil), http.StatusServiceUnavailable)
					if refusedBody.reads.Load() != 0 {
						t.Fatal("refused request body was read")
					}
				}
				publish := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "other-runner", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ChangeVersionInput: input, SourceBundle: bundle}
				other := newHubAPIRequest(t, http.MethodPost, f.path+"/versions", runners[1].redemption.Credential, publish)
				body, err := io.ReadAll(other.Body)
				if err != nil {
					t.Fatal(err)
				}
				other.Body = io.NopCloser(bytes.NewReader(body))
				requireNativeStatus(t, serve(other, body), http.StatusOK)
			})
		}
	}
}

func TestLargeRequestHubBoundAndRelease(t *testing.T) {
	var admission RequestAdmission
	e := echo.New()
	release := make(chan struct{})
	var workers sync.WaitGroup
	t.Cleanup(func() { close(release); workers.Wait() })
	path := "/api/v2/organizations/org_example/projects/prj_example/work-items/wi_example/changes/change_example/versions"
	for i := range hubLargeRequestLimit {
		started := make(chan struct{})
		c := e.NewContext(httptest.NewRequest(http.MethodPost, path, nil), httptest.NewRecorder())
		workers.Go(func() {
			if err := admission.Admit(c, strconv.Itoa(i), func(c echo.Context) error { close(started); <-release; return io.ErrUnexpectedEOF }); err != io.ErrUnexpectedEOF {
				t.Errorf("handler result = %v", err)
			}
		})
		<-started
	}
	response := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, path, nil), response)
	called := false
	if err := admission.Admit(c, "overflow", func(echo.Context) error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, response, http.StatusServiceUnavailable)
	if called {
		t.Fatal("Hub admitted a large request above its bound")
	}
	close(release)
	workers.Wait()
	release = make(chan struct{})
	for i := range hubLargeRequestLimit + 1 {
		c := e.NewContext(httptest.NewRequest(http.MethodPost, path, nil), httptest.NewRecorder())
		called = false
		if err := admission.Admit(c, strconv.Itoa(i), func(echo.Context) error { called = true; return nil }); err != nil {
			t.Fatal(err)
		}
		if !called {
			t.Fatal("finished request retained its reservation")
		}
	}
}
