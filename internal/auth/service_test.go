package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
)

func TestMagicLinkSessionLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := &memoryAuthStore{}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	sender := &recordingSender{}
	service := newAuthService(t, backend, sender, func() time.Time { return now })

	if err := service.RequestLink(ctx, "intruder@example.com", "/fleet"); err != nil {
		t.Fatalf("RequestLink(non-allowed) error = %v", err)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("non-allowed sender messages = %d, want 0", len(sender.messages))
	}
	if err := service.RequestLink(ctx, "Operator@Example.com", "/fleet?scope=all"); err != nil {
		t.Fatalf("RequestLink() error = %v", err)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("sender messages = %d, want 1", len(sender.messages))
	}
	message := sender.messages[0]
	if message.To != "operator@example.com" {
		t.Fatalf("message.To = %q, want operator@example.com", message.To)
	}
	link, err := url.Parse(message.URL)
	if err != nil {
		t.Fatalf("Parse(message.URL) error = %v", err)
	}
	rawToken := link.Query().Get("token")
	if rawToken == "" || link.Query().Get("next") != "/fleet?scope=all" {
		t.Fatalf("magic link = %q, want token and preserved next", message.URL)
	}
	assertOnlyTokenHashStored(t, backend, rawToken)

	sessionToken, session, err := service.ConsumeLink(ctx, rawToken)
	if err != nil {
		t.Fatalf("ConsumeLink() error = %v", err)
	}
	if session.Email != "operator@example.com" || sessionToken == "" {
		t.Fatalf("ConsumeLink() = token %q session %#v", sessionToken, session)
	}
	if _, _, err := service.ConsumeLink(ctx, rawToken); !errors.Is(err, auth.ErrInvalidLink) {
		t.Fatalf("ConsumeLink(reuse) error = %v, want %v", err, auth.ErrInvalidLink)
	}
	if _, err := service.Authenticate(ctx, sessionToken); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}

	service = newAuthService(t, backend, nil, func() time.Time { return now })
	if got, err := service.Authenticate(ctx, sessionToken); err != nil || got.Email != "operator@example.com" {
		t.Fatalf("Authenticate(new service) = %#v, %v", got, err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := service.Authenticate(ctx, sessionToken); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("Authenticate(expired) error = %v, want %v", err, auth.ErrInvalidSession)
	}
}

func TestSessionServiceAuthenticatesStoredSessionAndExpires(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := &memoryAuthStore{}
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	newService := func() *auth.Service {
		service, err := auth.NewSessionService(auth.SessionConfig{
			SessionTTL: time.Hour,
			PublicURL:  "https://detent.example.com",
		}, backend, auth.WithClock(func() time.Time { return now }))
		if err != nil {
			t.Fatalf("NewSessionService() error = %v", err)
		}
		return service
	}
	service := newService()
	token, session, err := service.CreateSession(ctx, "Operator@Example.com")
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if token == "" || session.Email != "operator@example.com" || !session.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("CreateSession() = %q, %#v", token, session)
	}
	service = newService()
	if got, err := service.Authenticate(ctx, token); err != nil || got.Email != session.Email {
		t.Fatalf("Authenticate(new service) = %#v, %v", got, err)
	}
	now = now.Add(time.Hour)
	if _, err := service.Authenticate(ctx, token); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("Authenticate(expired) error = %v, want %v", err, auth.ErrInvalidSession)
	}
}

func TestHostedIdentitySessionPreservesProviderLifetime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name    string
		modify  func(*auth.Identity)
		wantTTL time.Duration
		wantErr bool
	}{
		{name: "provider expires first", wantTTL: 10 * time.Minute},
		{name: "application expires first", modify: func(i *auth.Identity) { i.Hosted.ExpiresAt = now.Add(2 * time.Hour) }, wantTTL: time.Hour},
		{name: "support metadata preserved", modify: func(i *auth.Identity) {
			i.Hosted.SupportActor = "support@example.com"
			i.Hosted.SupportReason = "troubleshooting"
		}, wantTTL: 10 * time.Minute},
		{name: "unverified email", modify: func(i *auth.Identity) { i.EmailVerified = false }, wantErr: true},
		{name: "missing hosted identity", modify: func(i *auth.Identity) { i.Hosted = nil }, wantErr: true},
		{name: "subject mismatch", modify: func(i *auth.Identity) { i.Subject = "user_other" }, wantErr: true},
		{name: "expired provider", modify: func(i *auth.Identity) { i.Hosted.ExpiresAt = now }, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			identity := auth.Identity{
				Subject: "user_customer", Email: "Customer@Example.com", EmailVerified: true,
				Hosted: &auth.HostedIdentity{
					Subject: "user_customer", OrganizationID: "org_customer", SessionID: "session_customer",
					CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(10 * time.Minute),
				},
			}
			if tt.modify != nil {
				tt.modify(&identity)
			}
			backend := &recordingSessionStore{}
			service, err := auth.NewSessionService(auth.SessionConfig{SessionTTL: time.Hour, PublicURL: "https://tenant.example.com"}, backend, auth.WithClock(func() time.Time { return now }))
			if err != nil {
				t.Fatal(err)
			}
			token, session, err := service.CreateIdentitySession(t.Context(), identity)
			if tt.wantErr {
				if !errors.Is(err, auth.ErrInvalidSession) || backend.record.TokenHash != "" || token != "" {
					t.Fatalf("invalid identity created session: token=%q error=%v", token, err)
				}
				return
			}
			if err != nil || token == "" || session.Email != "customer@example.com" || session.Identity == nil || *session.Identity != *identity.Hosted || !session.ExpiresAt.Equal(now.Add(tt.wantTTL)) {
				t.Fatalf("CreateIdentitySession() session = %#v, error = %v", session, err)
			}
			sum := sha256.Sum256([]byte(token))
			if backend.record.TokenHash != hex.EncodeToString(sum[:]) || backend.record.Identity == nil || *backend.record.Identity != *identity.Hosted || !backend.record.ExpiresAt.Equal(session.ExpiresAt) {
				t.Fatal("persisted session lost provider identity, expiry, or token hashing")
			}
		})
	}
}

type recordingSessionStore struct {
	stubStore
	record auth.SessionRecord
}

func (s *recordingSessionStore) CreateWebSession(_ context.Context, record auth.SessionRecord) error {
	s.record = record
	return nil
}

func TestMagicLinkExpirationAndAllowlist(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := &memoryAuthStore{}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	service := newAuthService(t, backend, nil, func() time.Time { return now })

	tests := []struct {
		name  string
		email string
		want  error
	}{
		{name: "allowed address is case insensitive", email: "OPERATOR@example.com"},
		{name: "other address is rejected", email: "other@example.com", want: auth.ErrEmailNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			link, _, err := service.CreateLink(ctx, tt.email, "/")
			if !errors.Is(err, tt.want) {
				t.Fatalf("CreateLink() error = %v, want %v", err, tt.want)
			}
			if tt.want != nil {
				return
			}
			parsed, parseErr := url.Parse(link)
			if parseErr != nil {
				t.Fatalf("Parse(link) error = %v", parseErr)
			}
			now = now.Add(16 * time.Minute)
			if _, _, consumeErr := service.ConsumeLink(ctx, parsed.Query().Get("token")); !errors.Is(consumeErr, auth.ErrInvalidLink) {
				t.Fatalf("ConsumeLink(expired) error = %v, want %v", consumeErr, auth.ErrInvalidLink)
			}
		})
	}
}

func TestNewServiceValidation(t *testing.T) {
	t.Parallel()

	valid := auth.Config{
		AllowedEmails: []string{"operator@example.com"},
		LinkTTL:       time.Minute,
		SessionTTL:    time.Hour,
		PublicURL:     "https://detent.example.com",
	}
	tests := []struct {
		name  string
		cfg   auth.Config
		store auth.Store
	}{
		{name: "missing store", cfg: valid},
		{name: "missing allowlist", cfg: auth.Config{LinkTTL: time.Minute, SessionTTL: time.Hour, PublicURL: "https://detent.example.com"}, store: stubStore{}},
		{name: "invalid public url", cfg: auth.Config{AllowedEmails: valid.AllowedEmails, LinkTTL: time.Minute, SessionTTL: time.Hour, PublicURL: "/relative"}, store: stubStore{}},
		{name: "invalid link ttl", cfg: auth.Config{AllowedEmails: valid.AllowedEmails, SessionTTL: time.Hour, PublicURL: valid.PublicURL}, store: stubStore{}},
		{name: "invalid session ttl", cfg: auth.Config{AllowedEmails: valid.AllowedEmails, LinkTTL: time.Minute, PublicURL: valid.PublicURL}, store: stubStore{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := auth.NewService(tt.cfg, tt.store, nil); err == nil {
				t.Fatal("NewService() error = nil, want validation error")
			}
		})
	}
}

func TestNewSMTPSenderValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  auth.SMTPConfig
		want bool
	}{
		{name: "valid unauthenticated sender", cfg: auth.SMTPConfig{Host: "127.0.0.1", Port: 2525, From: "detent@example.com"}, want: true},
		{name: "valid authenticated sender", cfg: auth.SMTPConfig{Host: "smtp.example.com", Port: 587, Username: "user", Password: "secret", From: "detent@example.com"}, want: true},
		{name: "missing host", cfg: auth.SMTPConfig{Port: 587, From: "detent@example.com"}},
		{name: "invalid port", cfg: auth.SMTPConfig{Host: "smtp.example.com", Port: 70000, From: "detent@example.com"}},
		{name: "invalid from", cfg: auth.SMTPConfig{Host: "smtp.example.com", Port: 587, From: "invalid"}},
		{name: "partial credentials", cfg: auth.SMTPConfig{Host: "smtp.example.com", Port: 587, Username: "user", From: "detent@example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := auth.NewSMTPSender(tt.cfg)
			if (err == nil) != tt.want {
				t.Fatalf("NewSMTPSender() error = %v, success want %t", err, tt.want)
			}
		})
	}
}

type recordingSender struct {
	messages []auth.Message
}

func (s *recordingSender) SendMagicLink(_ context.Context, message auth.Message) error {
	s.messages = append(s.messages, message)
	return nil
}

type stubStore struct{}

func (stubStore) CreateMagicLink(context.Context, auth.MagicLink) error { return nil }
func (stubStore) ConsumeMagicLink(context.Context, auth.MagicLinkConsumption) (auth.Session, error) {
	return auth.Session{}, nil
}
func (stubStore) CreateWebSession(context.Context, auth.SessionRecord) error { return nil }
func (stubStore) WebSession(context.Context, string, time.Time) (auth.Session, error) {
	return auth.Session{}, nil
}

type memoryAuthStore struct {
	mu       sync.Mutex
	links    map[string]auth.MagicLink
	sessions map[string]auth.SessionRecord
}

func (s *memoryAuthStore) CreateMagicLink(_ context.Context, link auth.MagicLink) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.links == nil {
		s.links = make(map[string]auth.MagicLink)
	}
	s.links[link.TokenHash] = link
	return nil
}

func (s *memoryAuthStore) ConsumeMagicLink(_ context.Context, consumption auth.MagicLinkConsumption) (auth.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	link, ok := s.links[consumption.TokenHash]
	if !ok || !link.ExpiresAt.After(consumption.Now) {
		return auth.Session{}, auth.ErrInvalidLink
	}
	delete(s.links, consumption.TokenHash)
	if s.sessions == nil {
		s.sessions = make(map[string]auth.SessionRecord)
	}
	s.sessions[consumption.SessionHash] = auth.SessionRecord{TokenHash: consumption.SessionHash, Email: link.Email, ExpiresAt: consumption.SessionExpiresAt, CreatedAt: consumption.Now}
	return auth.Session{Email: link.Email, ExpiresAt: consumption.SessionExpiresAt}, nil
}

func (s *memoryAuthStore) CreateWebSession(_ context.Context, record auth.SessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[string]auth.SessionRecord)
	}
	s.sessions[record.TokenHash] = record
	return nil
}

func (s *memoryAuthStore) WebSession(_ context.Context, hash string, now time.Time) (auth.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.sessions[hash]
	if !ok || !record.ExpiresAt.After(now) {
		return auth.Session{}, auth.ErrInvalidSession
	}
	return auth.Session{Email: record.Email, ExpiresAt: record.ExpiresAt, Identity: record.Identity}, nil
}

func newAuthService(t *testing.T, backend auth.Store, sender auth.Sender, now func() time.Time) *auth.Service {
	t.Helper()
	service, err := auth.NewService(auth.Config{
		AllowedEmails: []string{"operator@example.com"},
		LinkTTL:       15 * time.Minute,
		SessionTTL:    time.Hour,
		PublicURL:     "https://detent.example.com",
	}, backend, sender, auth.WithClock(now))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}

func assertOnlyTokenHashStored(t *testing.T, backend *memoryAuthStore, rawToken string) {
	t.Helper()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	sum := sha256.Sum256([]byte(rawToken))
	want := hex.EncodeToString(sum[:])
	link, ok := backend.links[want]
	if !ok || len(backend.links) != 1 || link.TokenHash != want || link.TokenHash == rawToken {
		t.Fatalf("stored magic links = %#v, want one keyed by SHA-256 %q", backend.links, want)
	}
}
