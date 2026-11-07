package postgres

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

func TestAccessAuthorizationUnavailablePrecedesMissing(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{"missing", corestore.ErrNotFound, accessstore.ErrDenied},
		{"unavailable", tenantauth.ErrUnavailable, tenantauth.ErrUnavailable},
		{"unavailable then missing", errors.Join(tenantauth.ErrUnavailable, corestore.ErrNotFound), tenantauth.ErrUnavailable},
		{"missing then unavailable", errors.Join(corestore.ErrNotFound, tenantauth.ErrUnavailable), tenantauth.ErrUnavailable},
		{"lookup failure", errors.New("private-outage-sentinel"), tenantauth.ErrUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := accessAuthorizationError(test.err)
			if !errors.Is(err, test.want) || test.want == tenantauth.ErrUnavailable && errors.Is(err, accessstore.ErrDenied) {
				t.Fatalf("classification=%v, want %v", err, test.want)
			}
		})
	}
}
