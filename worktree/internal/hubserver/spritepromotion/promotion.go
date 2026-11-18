package spritepromotion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const secretKind = "fly_sprites_token"

type Deps struct {
	Organization tracker.OrganizationID
	Keys         *hubsecrets.Keyring
	Now          func() time.Time
	Read         func(context.Context, func(tenantstore.Reader) error) error
	Write        func(context.Context, func(tenantstore.Writer) error) error
	Audit        func(string, int) accessstore.SecretAudit
	Decode       func(json.RawMessage, any) error
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

type Change struct {
	ProjectID   tracker.ProjectID `json:"project_id"`
	Fingerprint string            `json:"fingerprint"`
}

func firstProjectToken(ctx context.Context, r accessstore.Reader) (Change, accessstore.Secret, error) {
	secret, err := r.FirstProjectAccessSecret(ctx, secretKind)
	if err != nil {
		return Change{}, secret, err
	}
	digest := sha256.Sum256(secret.Envelope.Ciphertext)
	return Change{ProjectID: secret.ProjectID, Fingerprint: hex.EncodeToString(digest[:])}, secret, nil
}

func (s *Service) Preview(ctx context.Context) (Change, error) {
	var change Change
	err := s.deps.Read(ctx, func(r tenantstore.Reader) error {
		if _, err := r.AccessSecret(ctx, "", secretKind); err == nil {
			return hubapi.PublicInvalid("An organization Sprites token is already configured")
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		var err error
		change, _, err = firstProjectToken(ctx, r)
		if errors.Is(err, corestore.ErrNotFound) {
			return hubapi.PublicInvalid("No existing project Sprites token is available")
		}
		return err
	})
	return change, err
}

func (s *Service) Execute(ctx context.Context, raw json.RawMessage) (chat.ActionExecution, error) {
	var change Change
	if s.deps.Decode(raw, &change) != nil || change.ProjectID == "" || change.Fingerprint == "" {
		return chat.ActionExecution{}, operatortool.ErrInvalidArguments
	}
	if s.deps.Keys == nil {
		return chat.ActionExecution{}, hubsecrets.ErrUnavailable
	}
	err := s.deps.Write(ctx, func(w tenantstore.Writer) error {
		if _, err := w.AccessSecret(ctx, "", secretKind); err == nil {
			return hubapi.PublicInvalid("An organization Sprites token is already configured")
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		current, source, err := firstProjectToken(ctx, w)
		if errors.Is(err, corestore.ErrNotFound) {
			return hubapi.PublicInvalid("The source Sprites token changed; request a new preview")
		}
		if err != nil {
			return err
		}
		if current != change {
			return hubapi.PublicInvalid("The source Sprites token changed; request a new preview")
		}
		token, err := s.deps.Keys.Open(source.Envelope, accessstore.SecretAAD(s.deps.Organization, source.ProjectID, secretKind))
		if err != nil {
			return err
		}
		defer clear(token)
		promoted, err := s.deps.Keys.Seal(token, accessstore.SecretAAD(s.deps.Organization, "", secretKind))
		if err != nil {
			return err
		}
		secret := accessstore.Secret{OrganizationID: s.deps.Organization, Kind: secretKind, OrganizationSlug: source.OrganizationSlug, Envelope: promoted, UpdatedAt: s.deps.Now()}
		err = w.PromoteAccessSecret(ctx, source, secret, s.deps.Audit("set", promoted.Version))
		if errors.Is(err, corestore.ErrUniqueViolation) {
			return hubapi.PublicInvalid("An organization Sprites token is already configured")
		}
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return hubapi.PublicInvalid("The source Sprites token changed; request a new preview")
		}
		return err
	})
	if err != nil {
		return chat.ActionExecution{}, err
	}
	return chat.ActionExecution{Message: "Organization Sprites token configured from the confirmed project token. All project overrides are preserved.", ResourceID: string(s.deps.Organization)}, nil
}
