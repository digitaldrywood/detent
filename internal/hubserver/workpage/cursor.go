package workpage

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Deps struct {
	Repository tenantstore.Repository
	Keys       *hubsecrets.Keyring
	Now        func() time.Time
}
type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

type Caller struct {
	Organization tracker.OrganizationID
	CredentialID string
	HostedRole   string
	PrincipalID  string
	Hosted       bool
	SessionID    string
}
type Cursor struct {
	Version    int            `json:"v"`
	Scope      string         `json:"scope"`
	After      string         `json:"after"`
	Expires    int64          `json:"expires"`
	Issues     *IssuePosition `json:"issues,omitempty"`
	Compaction int64          `json:"compaction,omitempty"`
}

type IssuePosition struct {
	Snapshot               string  `json:"snapshot"`
	Sequence               *int64  `json:"sequence,omitempty"`
	Score                  float64 `json:"score,omitempty"`
	Lane                   int     `json:"lane"`
	State                  string  `json:"state"`
	Priority               int     `json:"priority"`
	ActivityMissing        int     `json:"activity_missing"`
	Activity               string  `json:"activity"`
	Identifier             string  `json:"identifier"`
	SearchExact            bool    `json:"search_exact,omitempty"`
	SearchNumber           int64   `json:"search_number,omitempty"`
	SearchWindow           int64   `json:"search_window,omitempty"`
	SearchFingerprintAfter string  `json:"search_fingerprint_after,omitempty"`
}

func (s *Service) ReadLimit(ctx context.Context, scope Caller, path string, params url.Values, maximum, defaultLimit int) (int, Cursor, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	limit := defaultLimit
	var err error
	if value := strings.TrimSpace(params.Get("limit")); value != "" {
		limit, err = strconv.Atoi(value)
	}
	if limit < 1 || limit > maximum {
		err = hubapi.PublicInvalid("Page limit is invalid")
	}
	if err != nil {
		return 0, Cursor{}, nil, hubapi.PublicInvalid("Page limit is invalid")
	}
	requestedCursor := params.Get("cursor")
	cloned := url.Values{}
	for k, v := range params {
		cloned[k] = v
	}
	params = cloned
	params.Del("cursor")
	params.Del("limit")
	params.Del("archive_url")

	fingerprint := scope.CredentialID + " " + path + "?" + params.Encode()
	if scope.HostedRole != "" {
		fingerprint += " " + scope.HostedRole + " " + scope.PrincipalID
	}
	if scope.Hosted {
		fingerprint += " " + scope.SessionID
	}
	cursor := Cursor{Version: 2, Scope: fingerprint, Expires: s.deps.Now().Add(time.Hour).Unix()}
	key, err := s.ReadKey(ctx, scope.Organization)
	if err != nil {
		return 0, cursor, nil, err
	}
	value := requestedCursor
	if value == "" {
		return limit, cursor, key, nil
	}
	parts := strings.Split(value, ".")
	if len(parts) != 2 || len(value) > 4096 {
		return 0, cursor, nil, hubapi.PublicInvalid("Cursor is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return 0, cursor, nil, hubapi.PublicInvalid("Cursor is invalid")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, cursor, nil, hubapi.PublicInvalid("Cursor is invalid")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return 0, cursor, nil, hubapi.PublicInvalid("Cursor signature is invalid")
	}
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.Version != 2 || cursor.Scope != fingerprint || cursor.Expires <= s.deps.Now().Unix() {
		return 0, cursor, nil, hubapi.PublicInvalid("Cursor scope changed or expired; restart the query")
	}
	return limit, cursor, key, nil
}

func (s *Service) ReadKey(ctx context.Context, organization tracker.OrganizationID) ([]byte, error) {
	if s.deps.Repository == nil {
		return nil, hubapi.RunEventStorageUnavailable()
	}
	var secret accessstore.Secret
	err := s.deps.Repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		secret, err = r.AccessSecret(ctx, "", accessstore.NativeCursorKeyKind)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		repository, ok := s.deps.Repository.(accessstore.OrganizationSecretRepository)
		if !ok {
			return nil, hubapi.RunEventStorageUnavailable()
		}
		candidate, audit, createErr := NewSecret(s.deps.Keys, s.deps.Now, organization)
		if createErr != nil {
			return nil, createErr
		}
		secret, err = repository.EnsureOrganizationSecret(ctx, candidate, audit)
	}
	if err != nil {
		return nil, err
	}
	key, err := accessstore.OpenSecret(s.deps.Keys, secret)
	if err != nil {
		return nil, err
	}
	if len(key) != sha256.Size {
		return nil, errors.New("native cursor key has invalid length")
	}
	return key, nil
}

func NewSecret(keys *hubsecrets.Keyring, nowFunc func() time.Time, organization tracker.OrganizationID) (accessstore.Secret, accessstore.SecretAudit, error) {
	key := make([]byte, sha256.Size)
	defer clear(key)
	if _, err := rand.Read(key); err != nil {
		return accessstore.Secret{}, accessstore.SecretAudit{}, fmt.Errorf("generate native cursor key: %w", err)
	}
	envelope, err := keys.Seal(key, accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		return accessstore.Secret{}, accessstore.SecretAudit{}, fmt.Errorf("seal native cursor key: %w", err)
	}
	now := nowFunc().UTC()
	return accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now},
		accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "system:native-cursor-key", Event: "create", KeyVersion: envelope.Version, RecordedAt: now}, nil
}

func Encode(cursor Cursor, key []byte) (string, error) {
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
