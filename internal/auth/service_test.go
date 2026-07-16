package auth_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestMagicLinkSessionLifecyclePersists(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "detent.db")
	backend := openAuthStore(t, dbPath)
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() cleanup error = %v", err)
		}
	})
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
	assertTokenHashedAtRest(t, dbPath, rawToken)

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

	if err := backend.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	backend = openAuthStore(t, dbPath)
	service = newAuthService(t, backend, nil, func() time.Time { return now })
	if got, err := service.Authenticate(ctx, sessionToken); err != nil || got.Email != "operator@example.com" {
		t.Fatalf("Authenticate(after reopen) = %#v, %v", got, err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := service.Authenticate(ctx, sessionToken); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("Authenticate(expired) error = %v, want %v", err, auth.ErrInvalidSession)
	}
}

func TestMagicLinkExpirationAndAllowlist(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openAuthStore(t, filepath.Join(t.TempDir(), "detent.db"))
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() cleanup error = %v", err)
		}
	})
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

func openAuthStore(t *testing.T, path string) store.Store {
	t.Helper()
	backend, err := store.Open(context.Background(), store.Config{Backend: store.BackendSQLite, Path: path})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	return backend
}

func newAuthService(t *testing.T, backend store.Store, sender auth.Sender, now func() time.Time) *auth.Service {
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

func assertTokenHashedAtRest(t *testing.T, path string, rawToken string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	var stored string
	if err := db.QueryRow("SELECT token_hash FROM auth_magic_links ORDER BY id DESC LIMIT 1").Scan(&stored); err != nil {
		t.Fatalf("query token hash error = %v", err)
	}
	sum := sha256.Sum256([]byte(rawToken))
	want := hex.EncodeToString(sum[:])
	if stored != want || stored == rawToken {
		t.Fatalf("stored token = %q, want SHA-256 %q", stored, want)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close() error = %v", err)
	}
}
