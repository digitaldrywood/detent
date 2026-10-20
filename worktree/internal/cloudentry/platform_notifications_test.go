package cloudentry

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

type platformMailSender struct {
	messages chan auth.EmailMessage
	release  chan struct{}
	err      error
}

func (s *platformMailSender) SendEmail(ctx context.Context, message auth.EmailMessage) error {
	s.messages <- message
	select {
	case <-s.release:
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func platformMailConfig(t *testing.T, sender auth.EmailSender, output *logBuffer) Config {
	t.Helper()
	return Config{
		Platform:  PlatformConfig{BootstrapAdminEmail: "admin-a@example.test"},
		PublicURL: "https://cloud.example.test", ListenAddress: "127.0.0.1:0", Issuer: "entry",
		SigningKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), Provider: newFakeProvider(),
		StateDir: t.TempDir(), clientFS: fstest.MapFS{}, platformEmailSender: sender,
		Logger: slog.New(slog.NewJSONHandler(output, nil)),
		now:    func() time.Time { return time.Date(2026, 10, 19, 16, 0, 0, 0, time.UTC) },
	}
}

func TestPlatformNotifications(t *testing.T) {
	for _, test := range []struct {
		name, event, key string
		sendError        bool
		historical       bool
		paid             bool
		customBase       bool
	}{
		{name: "signup", event: "signup", key: "fake-key"},
		{name: "organization", event: "organization", key: "fake-key"},
		{name: "custom base organization", event: "organization", key: "fake-key", customBase: true},
		{name: "paid organization", event: "organization", key: "fake-key", paid: true},
		{name: "signup sender error", event: "signup", key: "fake-key", sendError: true},
		{name: "organization sender error", event: "organization", key: "fake-key", sendError: true},
		{name: "returning identities", event: "signup", key: "fake-key", historical: true},
		{name: "disabled signup", event: "signup"},
		{name: "disabled organization", event: "organization"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("RESEND_API_KEY", test.key)
			var output logBuffer
			sender := &platformMailSender{messages: make(chan auth.EmailMessage, 16), release: make(chan struct{})}
			if test.sendError {
				sender.err = errors.New("fixture mail failure")
			}
			cfg := platformMailConfig(t, sender, &output)
			s, err := Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			s.config.Allocation = &AllocationConfig{SocketRoot: filepath.Join(t.TempDir(), "s"), Prices: []hubserver.HostedBillingPrice{{PriceID: "price_team", Plan: hubserver.PlanReference{ID: "team", Version: 1}}}}
			if test.customBase {
				s.config.Runtime = &hubserver.HostedRuntimeUpdate{Plans: &hubserver.HostedPlansConfig{Base: hubserver.PlanReference{ID: "pilot_free", Version: 1}}}
			}
			if test.paid {
				s.config.Billing = &BillingConfig{}
			}
			for _, member := range []struct{ email, role string }{
				{"admin-a@example.test", "admin"}, {"admin-b@example.test", "admin"},
				{"support@example.test", "support"}, {"viewer@example.test", "viewer"}, {"billing@example.test", "billing"},
			} {
				if _, err := s.registry.store.db.ExecContext(t.Context(), "INSERT INTO platform_members VALUES(?,?,'fixture','time','time') ON CONFLICT(email) DO UPDATE SET role=excluded.role", member.email, member.role); err != nil {
					t.Fatal(err)
				}
			}
			startup := output.take()
			wantSkips := 0
			if test.key == "" {
				wantSkips = 1
			}
			if got := strings.Count(startup, "platform notifications skipped"); got != wantSkips {
				t.Fatalf("skip logs=%d want %d: %s", got, wantSkips, startup)
			}
			ids := map[string]string{}
			trigger := func(subject string) {
				t.Helper()
				email := subject + "@example.test"
				if test.event == "signup" {
					identity := auth.Identity{Subject: subject, Email: email, Hosted: &auth.HostedIdentity{Subject: subject, ExpiresAt: cfg.now().Add(time.Hour)}}
					recorder := httptest.NewRecorder()
					c := s.echo.NewContext(httptest.NewRequest(http.MethodGet, "/auth/oidc/callback", nil), recorder)
					if _, err := s.establishSession(c, identity); err != nil {
						t.Fatal(err)
					}
					if len(recorder.Result().Cookies()) == 0 {
						t.Fatal("sign-in did not set a session cookie")
					}
				} else {
					price := ""
					if test.paid {
						price = "price_team"
					}
					id, err := s.createOrganizationFor(t.Context(), accountSession{Subject: subject, Email: email}, "Example "+subject, "creation_key_"+subject, price)
					if err != nil {
						t.Fatal(err)
					}
					if prior := ids[subject]; prior != "" && prior != id {
						t.Fatal("organization replay changed ID")
					}
					ids[subject] = id
				}
			}
			for _, subject := range []string{"user_alpha", "user_beta"} {
				if test.historical {
					if err := s.auth.audit(t.Context(), subject, "", "session_started"); err != nil {
						t.Fatal(err)
					}
				}
				trigger(subject)
				trigger(subject)
			}
			close(sender.release)
			want := 4
			if test.key == "" || test.historical {
				want = 0
			}
			var messages []auth.EmailMessage
			for range want {
				select {
				case message := <-sender.messages:
					messages = append(messages, message)
				case <-t.Context().Done():
					t.Fatal("notification worker did not deliver")
				}
			}
			s.closePlatformNotifications()
			seen := map[string]int{}
			for _, message := range messages {
				if message.To != "admin-a@example.test" && message.To != "admin-b@example.test" {
					t.Fatalf("non-admin recipient: %s", message.To)
				}
				seen[message.To+message.Subject]++
				if !strings.Contains(message.Body, "Time: 2026-10-19T16:00:00Z") {
					t.Fatalf("missing event time: %s", message.Body)
				}
				matched := false
				for _, subject := range []string{"user_alpha", "user_beta"} {
					email := subject + "@example.test"
					if test.event == "signup" && message.Subject == "New sign-up: "+email {
						matched = strings.Contains(message.Body, cfg.PublicURL+"/platform/users/"+email)
					} else if test.event == "organization" && message.Subject == "New organization: Example "+subject {
						plan := "free"
						if test.customBase {
							plan = "pilot_free"
						}
						if test.paid {
							plan = "team"
						}
						matched = strings.Contains(message.Body, "Creator: "+email) && strings.Contains(message.Body, "Plan: "+plan) && strings.Contains(message.Body, cfg.PublicURL+"/platform/tenants/"+ids[subject])
					}
				}
				if !matched {
					t.Fatalf("incorrect notification: %+v", message)
				}
			}
			for key, count := range seen {
				if count != 1 {
					t.Fatalf("duplicate %s: %d", key, count)
				}
			}
			logs := output.take()
			wantErrors := 0
			if test.sendError {
				wantErrors = 2
			}
			if got := strings.Count(logs, "platform notification delivery failed"); got != wantErrors {
				t.Fatalf("failure logs=%d want %d: %s", got, wantErrors, logs)
			}
			if test.sendError && !strings.Contains(logs, "fixture mail failure") {
				t.Fatalf("missing error: %s", logs)
			}
			if strings.Contains(logs, "platform notifications skipped") {
				t.Fatal("skip logged more than once")
			}
			allocation, billing, runtime := s.config.Allocation, s.config.Billing, s.config.Runtime
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			s.closePlatformNotifications()
			s.config.Allocation, s.config.Billing, s.config.Runtime = allocation, billing, runtime
			for _, subject := range []string{"user_alpha", "user_beta"} {
				trigger(subject)
			}
			if test.key != "" {
				s.deliverPlatformNotifications(t.Context())
			}
			select {
			case message := <-sender.messages:
				t.Fatalf("unexpected or replayed mail: %+v", message)
			default:
			}
		})
	}
}

func TestPlatformNotificationRecipientsAtSendTime(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "fake-key")
	var output logBuffer
	sender := &platformMailSender{messages: make(chan auth.EmailMessage, 4), release: make(chan struct{})}
	close(sender.release)
	cfg := platformMailConfig(t, sender, &output)
	cfg.Platform.BootstrapAdminEmail = "former-admin@example.test"
	s, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	s.closePlatformNotifications()
	for i := range 2 {
		identity := auth.Identity{Subject: fmt.Sprintf("subject_%d", i), Email: fmt.Sprintf("user%d@example.test", i), Hosted: &auth.HostedIdentity{}}
		if err := s.recordSignIn(t.Context(), identity); err != nil {
			t.Fatal(err)
		}
	}
	for _, member := range []struct{ email, role string }{{"new-admin@example.test", "admin"}, {"former-admin@example.test", "viewer"}} {
		if _, err := s.registry.store.db.ExecContext(t.Context(), "INSERT INTO platform_members VALUES(?,?,'fixture','time','time') ON CONFLICT(email) DO UPDATE SET role=excluded.role", member.email, member.role); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Platform.BootstrapAdminEmail = ""
	s, err = Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case message := <-sender.messages:
			if message.To != "new-admin@example.test" {
				t.Fatalf("stale recipients: %+v", message)
			}
		case <-t.Context().Done():
			t.Fatal("pending notification was lost across restart")
		}
	}
}
