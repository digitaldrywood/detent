package tenantcall

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry/tenant"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

const AssertionLifetime = 20 * time.Second

type Deps struct {
	Issuer     string
	SigningKey ed25519.PrivateKey
	Now        func() time.Time
	Transport  func(tenant.Organization) (http.RoundTripper, error)
}

type Client struct{ deps Deps }

func New(deps Deps) *Client { return &Client{deps: deps} }

func (s *Client) Claims(organization tenant.Organization, kind, method, path string, body []byte) (cloudassert.Claims, error) {
	id, err := cloudassert.NewID()
	if err != nil {
		return cloudassert.Claims{}, err
	}
	now := s.deps.Now()
	return cloudassert.Claims{
		OrganizationSlug: organization.Slug, Issuer: s.deps.Issuer, Audience: organization.ID, Generation: organization.Generation, Kind: kind,
		Method: method, Path: path, BodyDigest: cloudassert.BodyDigest(body), IssuedAt: now, ExpiresAt: now.Add(AssertionLifetime), ID: id,
	}, nil
}

func (s *Client) ServiceRequest(ctx context.Context, organization tenant.Organization, path string, payload any, identity func(*cloudassert.Claims)) (int, error) {
	status, _, err := s.ServiceCall(ctx, organization, path, payload, identity)
	return status, err
}

func (s *Client) ServiceCall(ctx context.Context, organization tenant.Organization, path string, payload any, identity func(*cloudassert.Claims)) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	claims, err := s.Claims(organization, cloudassert.KindService, http.MethodPost, path, body)
	if err != nil {
		return 0, nil, err
	}
	if identity != nil {
		identity(&claims)
	}
	return s.SignedCall(ctx, organization, claims, body, "")
}

const tenantCallResponseLimit = 64 << 10

func (s *Client) MachineCall(ctx context.Context, organization tenant.Organization, method, path string, body []byte, bearer string) (int, []byte, error) {
	claims, err := s.Claims(organization, cloudassert.KindMachine, method, path, body)
	if err != nil {
		return 0, nil, err
	}
	return s.SignedCall(ctx, organization, claims, body, bearer)
}

func (s *Client) SignedCall(ctx context.Context, organization tenant.Organization, claims cloudassert.Claims, body []byte, bearer string) (int, []byte, error) {
	return s.SignedCallCSRF(ctx, organization, claims, body, bearer, "")
}

func (s *Client) SignedCallCSRF(ctx context.Context, organization tenant.Organization, claims cloudassert.Claims, body []byte, bearer, csrf string) (int, []byte, error) {
	assertion, err := cloudassert.Sign(s.deps.SigningKey, claims)
	if err != nil {
		return 0, nil, err
	}
	transport, err := s.deps.Transport(organization)
	if err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, claims.Method, "http://tenant"+claims.Path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set(echo.HeaderAuthorization, "Bearer "+bearer)
	}
	request.Header.Set(cloudassert.Header, assertion)
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	timeout := 15 * time.Second
	response, err := (&http.Client{Transport: transport, Timeout: timeout}).Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	limit := int64(tenantCallResponseLimit)
	if claims.Path == "/internal/v1/keys/catalog" || claims.Path == "/internal/v1/keys/call" {
		limit = mcp.MaxHTTPResponseBytes
	}
	if claims.Path == "/internal/v1/keys/call" {
		var call struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(body, &call) == nil {
			limit = max(limit, int64(operatortool.ResultByteLimit(call.Name)))
		}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return 0, nil, err
	}
	if int64(len(raw)) > limit {
		return 0, nil, fmt.Errorf("tenant %s %s response exceeds %d bytes", request.Method, request.URL.Path, limit)
	}
	return response.StatusCode, raw, nil
}
