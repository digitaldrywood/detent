package hubtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HTTPOptions struct {
	Repository   tenantstore.Repository
	Organization tracker.OrganizationID
	Now          func() time.Time
	Hosted       *hubauth.Hosted
}

type HTTPDeps struct {
	API      func(...hubauth.TokenScope) echo.MiddlewareFunc
	Auth     *hubauth.Authenticator
	Operator echo.MiddlewareFunc
	Native   func(...hubauth.TokenScope) echo.MiddlewareFunc
	Error    func(echo.Context, error) error
}

type HTTPHarness struct {
	Echo         *echo.Echo
	Repository   tenantstore.Repository
	Organization tracker.OrganizationID
	Now          func() time.Time
	Deps         HTTPDeps
	mu           sync.Mutex
	memberships  map[string]auth.Membership
	hosted       *hubauth.Hosted
}

func NewHTTP(t *testing.T, options HTTPOptions, register func(*echo.Group, HTTPDeps)) *HTTPHarness {
	t.Helper()
	if options.Repository == nil {
		options.Repository = Repository(t)
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Organization == "" {
		options.Organization = Organization(t, options.Repository, OrganizationOptions{Now: options.Now()}).OrganizationID
	}
	h := &HTTPHarness{Echo: echo.New(), Repository: options.Repository, Organization: options.Organization, Now: options.Now, memberships: map[string]auth.Membership{}}
	if options.Hosted != nil {
		hosted := *options.Hosted
		hosted.StaffEmails = append([]string(nil), hosted.StaffEmails...)
		if hosted.OrganizationID == "" {
			hosted.OrganizationID = string(h.Organization)
		}
		h.hosted = &hosted
	}
	var a *hubauth.Authenticator
	sessionStore := httpSessionStore{read: func(ctx context.Context, hash string, now time.Time) (auth.Session, error) {
		return a.WebSession(ctx, hash, now)
	}}
	sessions, err := auth.NewSessionService(auth.SessionConfig{SessionTTL: time.Hour, PublicURL: "https://example.test"}, sessionStore, auth.WithClock(h.Now))
	if err != nil {
		t.Fatal(err)
	}
	a = hubauth.New(hubauth.Deps{Repository: h.Repository, AccessRepository: h.Repository, Organization: h.Organization, Now: h.Now, Hosted: h.hosted,
		SessionLookup: sessions.Authenticate,
		CurrentSession: func(_ context.Context, identity auth.HostedIdentity) (auth.HostedIdentity, error) {
			return identity, nil
		},
		Memberships: func(ctx context.Context, subject, organization string) ([]auth.Membership, error) {
			membership, err := h.membership(ctx, &auth.HostedIdentity{Subject: subject, OrganizationID: organization})
			if errors.Is(err, auth.ErrHostedIdentity) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return []auth.Membership{membership}, nil
		},
	})
	h.Deps = HTTPDeps{Auth: a, Error: HTTPError}
	h.Deps.API = func(roles ...hubauth.TokenScope) echo.MiddlewareFunc { return a.RequireAPI(h.apiOptions(), roles...) }
	h.Deps.Operator = a.Operator(hubauth.OperatorOptions{
		Classify: hubauth.Classify, Error: HTTPError, Refuse: httpRefuse,
		AccountCredential: func(context.Context, string) (hubauth.Credential, error) {
			return hubauth.Credential{}, auth.ErrHostedIdentity
		},
		CurrentSession: a.WebSession,
		AccessTime:     func(_ context.Context, now time.Time) time.Time { return now },
		Authority: func(ctx context.Context, c hubauth.Credential, organization string) (operatortool.Authority, error) {
			return a.CurrentAuthority(ctx, c, organization, a.DefaultAuthorityOptions())
		},
		Catalog: func(ctx context.Context, _ hubauth.Credential, _ string) context.Context { return ctx },
	})
	h.Deps.Native = func(roles ...hubauth.TokenScope) echo.MiddlewareFunc {
		return a.Native(hubauth.NativeMiddlewareOptions{API: h.apiOptions(), Authorize: func(c echo.Context) error {
			return a.AuthorizeRead(c, func(ctx context.Context) error { return a.ResolveNative(ctx, c, h.nativeOptions(c)) })
		}}, roles...)
	}
	h.Echo.HTTPErrorHandler = func(err error, c echo.Context) {
		if !c.Response().Committed {
			if responseErr := HTTPError(c, err); responseErr != nil {
				t.Errorf("write HTTP error: %v", responseErr)
			}
		}
	}
	register(h.Echo.Group(""), h.Deps)
	return h
}

func (h *HTTPHarness) membership(_ context.Context, identity *auth.HostedIdentity) (auth.Membership, error) {
	if identity == nil {
		return auth.Membership{}, auth.ErrHostedIdentity
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	member, ok := h.memberships[identity.Subject]
	if !ok || member.OrganizationID != identity.OrganizationID || member.Status != "active" || !auth.ValidOrganizationRole(member.Role.Slug) {
		return auth.Membership{}, auth.ErrHostedIdentity
	}
	return member, nil
}

func (h *HTTPHarness) apiOptions() hubauth.APIOptions {
	return hubauth.APIOptions{Classify: hubauth.Classify, Error: HTTPError, Refuse: httpRefuse, IsNotFound: func(err error) bool {
		var failure *hubapi.Error
		return errors.As(err, &failure) && failure.Code == "not_found"
	},
		HostedAdministration: func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error { return HTTPError(c, operatortool.ErrAccessDenied) }
		},
		ArtifactPublisher: func(echo.Context, hubauth.Credential) bool { return false }, ChangePrincipal: func(echo.Context, hubauth.Credential) bool { return false }, ConfigurationRank: func(echo.Context) bool { return false },
		CompatibilityResource: func(echo.Context) error { return operatortool.ErrAccessDenied }, AdmitRunner: func(echo.Context, hubauth.Credential) error { return nil },
	}
}
func (h *HTTPHarness) nativeOptions(c echo.Context) hubauth.NativeOptions {
	a := h.Deps.Auth
	return hubauth.NativeOptions{Credential: hubauth.RequestCredential, EventScope: func(echo.Context, hubauth.Input) bool { return false }, ArtifactRead: func(echo.Context) bool { return false }, RelayTicket: func(echo.Context) bool { return false }, LeaseRoute: func(string) bool { return false }, RepositoryRoute: func(string) bool { return false }, RecordError: hubauth.RecordError,
		ReadProject: func(ctx context.Context, input hubauth.Input, write, models bool) error {
			return h.Repository.ReadTenant(ctx, input.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				if err := a.Authorize(ctx, r, input, write); err != nil {
					return err
				}
				_, err := r.ProjectVersion(ctx, input.Project)
				return hubauth.RecordError(err)
			})
		},
		ReadItem: func(ctx context.Context, input hubauth.Input, id string) error {
			return h.Repository.ReadTenant(ctx, input.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				item, err := r.WorkItem(ctx, input.Project, tracker.NativeWorkItemID(id))
				if err != nil {
					return hubauth.RecordError(err)
				}
				if input.Credential.HostedRole == "reporter" && hubapi.StringValue(item.ActorPrincipalID) != hubauth.Actor(input.Credential, input.Organization).PrincipalID {
					return hubapi.NotFound()
				}
				return nil
			})
		},
		Store: func(c echo.Context, input hubauth.Input) { c.Set("hub_request_scope", a.Scope(input)) },
	}
}

func httpRefuse(c echo.Context, status int, code, message string, body any) error {
	if body == nil {
		return c.NoContent(status)
	}
	return c.JSON(status, body)
}
func HTTPError(c echo.Context, err error) error {
	var failure *hubapi.Error
	if errors.As(err, &failure) {
		return c.JSON(failure.Status, failure)
	}
	if errors.Is(err, corestore.ErrNotFound) {
		return HTTPError(c, hubapi.NotFound())
	}
	if errors.Is(err, operatortool.ErrAccessDenied) {
		return c.JSON(http.StatusForbidden, hubauth.Response{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
	}
	if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) || errors.Is(err, auth.ErrInvalidSession) {
		return c.JSON(http.StatusForbidden, hubauth.Response{Code: "access_denied", Message: "Access is no longer available"})
	}
	return c.JSON(http.StatusServiceUnavailable, hubauth.Response{Code: "service_unavailable", Message: "Authorization is temporarily unavailable"})
}

type HTTPActor struct {
	Token      string
	Cookie     bool
	Member     SeededMember
	Credential SeededCredential
}

type HTTPResult struct {
	Status   int
	Body     json.RawMessage
	Recorder *httptest.ResponseRecorder
}

func (h *HTTPHarness) Request(t *testing.T, actor HTTPActor, method, target string, body any) HTTPResult {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequestWithContext(t.Context(), method, target, bytes.NewReader(encoded))
	if body != nil {
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	actor.Apply(request)
	response := httptest.NewRecorder()
	h.Echo.ServeHTTP(response, request)
	return HTTPResult{Status: response.Code, Body: append(json.RawMessage(nil), response.Body.Bytes()...), Recorder: response}
}
func (a HTTPActor) Apply(request *http.Request) {
	if a.Cookie {
		request.AddCookie(&http.Cookie{Name: hubauth.Cookie, Value: a.Token, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	} else if a.Token != "" {
		request.Header.Set(echo.HeaderAuthorization, "Bearer "+a.Token)
	}
}
func (r HTTPResult) Decode(t *testing.T, target any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, target); err != nil {
		t.Fatalf("decode HTTP status %d: %v: %s", r.Status, err, r.Body)
	}
}

type httpSessionStore struct {
	read func(context.Context, string, time.Time) (auth.Session, error)
}

func (s httpSessionStore) WebSession(ctx context.Context, hash string, now time.Time) (auth.Session, error) {
	return s.read(ctx, hash, now)
}
func (s httpSessionStore) CreateWebSession(context.Context, auth.SessionRecord) error {
	return auth.ErrInvalidSession
}
