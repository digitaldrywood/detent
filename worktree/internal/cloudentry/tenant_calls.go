package cloudentry

import (
	"context"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry/tenantcall"
)

func (s *Service) tenantCaller() *tenantcall.Client {
	return tenantcall.New(tenantcall.Deps{Issuer: s.config.Issuer, SigningKey: s.config.SigningKey, Now: s.config.Now, Transport: s.config.Transport})
}

func (s *Service) claims(organization Organization, kind, method, path string, body []byte) (cloudassert.Claims, error) {
	return s.tenantCaller().Claims(organization, kind, method, path, body)
}

func (s *Service) serviceRequest(ctx context.Context, organization Organization, path string, payload any, identity func(*cloudassert.Claims)) (int, error) {
	return s.tenantCaller().ServiceRequest(ctx, organization, path, payload, identity)
}

func (s *Service) serviceCall(ctx context.Context, organization Organization, path string, payload any, identity func(*cloudassert.Claims)) (int, []byte, error) {
	return s.tenantCaller().ServiceCall(ctx, organization, path, payload, identity)
}

func (s *Service) machineCall(ctx context.Context, organization Organization, method, path string, body []byte, bearer string) (int, []byte, error) {
	return s.tenantCaller().MachineCall(ctx, organization, method, path, body, bearer)
}

func (s *Service) signedCall(ctx context.Context, organization Organization, claims cloudassert.Claims, body []byte, bearer string) (int, []byte, error) {
	return s.tenantCaller().SignedCall(ctx, organization, claims, body, bearer)
}

func (s *Service) signedCallCSRF(ctx context.Context, organization Organization, claims cloudassert.Claims, body []byte, bearer, csrf string) (int, []byte, error) {
	return s.tenantCaller().SignedCallCSRF(ctx, organization, claims, body, bearer, csrf)
}
