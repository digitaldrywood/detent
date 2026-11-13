package hubserver

import (
	"context"
	"errors"
	"net"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type SecretCursor struct {
	ProjectID tracker.ProjectID
	Kind      string
}

func (s *Service) MaintainSecret(ctx context.Context, organization tracker.OrganizationID, after SecretCursor) (SecretCursor, bool, error) {
	if organization == "" || s.config.AccessRepository == nil {
		return after, false, corestore.ErrTenantMismatch
	}
	if s.config.SecretKeys == nil {
		return after, false, nil
	}
	var page []accessstore.Secret
	err := s.config.AccessRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		page, err = r.AccessSecrets(ctx, after.ProjectID, after.Kind, 1)
		return err
	})
	if err != nil {
		return after, false, err
	}
	if len(page) == 0 {
		return SecretCursor{}, false, nil
	}
	secret := page[0]
	next := SecretCursor{ProjectID: secret.ProjectID, Kind: secret.Kind}
	if secret.Envelope.Version == s.config.SecretKeys.Version() {
		return next, true, nil
	}
	envelope, err := s.config.SecretKeys.Rewrap(secret.Envelope, accessstore.SecretAAD(organization, secret.ProjectID, secret.Kind))
	if err != nil {
		return next, true, err
	}
	err = s.config.AccessRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		return w.RewrapAccessSecret(ctx, secret, envelope, accessstore.SecretAudit{OrganizationID: organization, ProjectID: secret.ProjectID, Kind: secret.Kind, Actor: "system:rewrap", Event: "rotate", KeyVersion: envelope.Version, RecordedAt: s.config.Clock()})
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		err = nil
	}
	return next, true, err
}

type secretRewrapListener struct {
	net.Listener
	serving func()
}

func (l secretRewrapListener) Accept() (net.Conn, error) {
	l.serving()
	return l.Listener.Accept()
}

func (s *Service) signalSecretRewrap() {
	s.secretRewrapOnce.Do(func() { close(s.secretRewrapReady) })
}

func (s *Service) runSecretRewrap(ctx context.Context) {
	defer close(s.secretRewrapDone)
	select {
	case <-ctx.Done():
		return
	case <-s.secretRewrapReady:
	}
	s.rewrapSecrets(ctx)
}

func (s *Service) rewrapSecrets(ctx context.Context) {
	if s.config.SecretKeys == nil {
		return
	}
	if err := s.rewrapAccessSecrets(ctx); err != nil && ctx.Err() == nil {
		s.config.Logger.Error("rewrap tenant secrets", "error", err)
	}
}
