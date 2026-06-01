package budget

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
)

func TestCheckerCheckDispatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	pricing := PricingTable{
		"gpt-test": {
			USDPerInputToken:  0.01,
			USDPerOutputToken: 0.02,
		},
	}

	tests := []struct {
		name       string
		cfg        Config
		spend      *fakeSpendStore
		req        DispatchRequest
		wantCode   ReasonCode
		wantSpend  float64
		wantMax    float64
		wantErr    string
		wantAllow  bool
		wantDaily  int
		wantIssues int
	}{
		{
			name:      "disabled budget allows without spend store",
			cfg:       Config{},
			req:       DispatchRequest{Model: "gpt-test", Now: now, Estimate: TokenEstimate{InputTokens: 10}},
			wantAllow: true,
		},
		{
			name: "missing model pricing allows without spend lookup",
			cfg: Config{
				Enabled:      true,
				PerDayMaxUSD: 0.01,
			},
			spend:     &fakeSpendStore{},
			req:       DispatchRequest{Model: "missing-model", Now: now, Estimate: TokenEstimate{InputTokens: 10}},
			wantAllow: true,
		},
		{
			name: "daily cap refuses when projection exceeds limit",
			cfg: Config{
				Enabled:         true,
				PerDayMaxUSD:    1.0,
				RefusalCooldown: time.Minute,
			},
			spend: &fakeSpendStore{
				daily: store.TokenSpend{
					ByModel: []store.ModelTokenSpend{
						{Model: "gpt-test", InputTokens: 95},
					},
				},
			},
			req: DispatchRequest{
				IssueID:    "issue-daily",
				Identifier: "MT-DAILY",
				Model:      "gpt-test",
				Now:        now,
				Estimate:   TokenEstimate{InputTokens: 10},
			},
			wantCode:  ReasonPerDayMaxUSD,
			wantSpend: 1.05,
			wantMax:   1.0,
			wantDaily: 1,
		},
		{
			name: "per issue cap refuses only matching issue spend",
			cfg: Config{
				Enabled:        true,
				PerIssueMaxUSD: 0.5,
			},
			spend: &fakeSpendStore{
				issues: map[string]store.TokenSpend{
					"issue-expensive|MT-EXPENSIVE|https://example.com/issue-expensive": {
						ByModel: []store.ModelTokenSpend{
							{Model: "gpt-test", InputTokens: 45},
						},
					},
				},
			},
			req: DispatchRequest{
				IssueID:    "issue-expensive",
				Identifier: "MT-EXPENSIVE",
				IssueURL:   "https://example.com/issue-expensive",
				Model:      "gpt-test",
				Now:        now,
				Estimate:   TokenEstimate{InputTokens: 10},
			},
			wantCode:   ReasonPerIssueMaxUSD,
			wantSpend:  0.55,
			wantMax:    0.5,
			wantIssues: 1,
		},
		{
			name: "per issue cap uses identifier when issue id is missing",
			cfg: Config{
				Enabled:        true,
				PerIssueMaxUSD: 0.5,
			},
			spend: &fakeSpendStore{
				issues: map[string]store.TokenSpend{
					"|MT-FALLBACK|": {
						ByModel: []store.ModelTokenSpend{
							{Model: "gpt-test", InputTokens: 45},
						},
					},
				},
			},
			req: DispatchRequest{
				Identifier: "MT-FALLBACK",
				Model:      "gpt-test",
				Now:        now,
				Estimate:   TokenEstimate{InputTokens: 10},
			},
			wantCode:   ReasonPerIssueMaxUSD,
			wantSpend:  0.55,
			wantMax:    0.5,
			wantIssues: 1,
		},
		{
			name: "caps allow when projected spend is below limits",
			cfg: Config{
				Enabled:        true,
				PerDayMaxUSD:   1.0,
				PerIssueMaxUSD: 1.0,
			},
			spend: &fakeSpendStore{},
			req: DispatchRequest{
				IssueID:  "issue-under",
				Model:    "gpt-test",
				Now:      now,
				Estimate: TokenEstimate{InputTokens: 10},
			},
			wantAllow:  true,
			wantDaily:  1,
			wantIssues: 1,
		},
		{
			name: "enabled cap requires spend store",
			cfg: Config{
				Enabled:      true,
				PerDayMaxUSD: 1.0,
			},
			req:     DispatchRequest{Model: "gpt-test", Now: now, Estimate: TokenEstimate{InputTokens: 10}},
			wantErr: "budget spend store is required",
		},
		{
			name: "spend errors are wrapped",
			cfg: Config{
				Enabled:      true,
				PerDayMaxUSD: 1.0,
			},
			spend: &fakeSpendStore{
				dailyErr: errors.New("database unavailable"),
			},
			req:       DispatchRequest{Model: "gpt-test", Now: now, Estimate: TokenEstimate{InputTokens: 10}},
			wantErr:   "daily token spend",
			wantDaily: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			checker := NewChecker(tt.cfg, tt.spend, pricing)
			decision, err := checker.CheckDispatch(ctx, tt.req)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("CheckDispatch() error = %v, want containing %q", err, tt.wantErr)
				}
				if tt.spend != nil {
					assertCallCounts(t, tt.spend, tt.wantDaily, tt.wantIssues)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckDispatch() error = %v", err)
			}

			if decision.Allowed != tt.wantAllow {
				t.Fatalf("Decision.Allowed = %v, want %v", decision.Allowed, tt.wantAllow)
			}
			if tt.wantCode == "" {
				if decision.Refusal != nil {
					t.Fatalf("Decision.Refusal = %#v, want nil", decision.Refusal)
				}
				if tt.spend != nil {
					assertCallCounts(t, tt.spend, tt.wantDaily, tt.wantIssues)
				}
				return
			}

			if decision.Refusal == nil {
				t.Fatal("Decision.Refusal = nil, want refusal")
			}
			refusal := *decision.Refusal
			if refusal.Code != tt.wantCode {
				t.Fatalf("Refusal.Code = %q, want %q", refusal.Code, tt.wantCode)
			}
			assertInDelta(t, refusal.ProjectedSpendUSD, tt.wantSpend)
			if refusal.MaxUSD == nil {
				t.Fatal("Refusal.MaxUSD = nil, want cap")
			}
			assertInDelta(t, *refusal.MaxUSD, tt.wantMax)
			if refusal.RefusedAt != now {
				t.Fatalf("Refusal.RefusedAt = %s, want %s", refusal.RefusedAt, now)
			}
			if refusal.CooldownUntil.Before(now) {
				t.Fatalf("Refusal.CooldownUntil = %s, want at or after %s", refusal.CooldownUntil, now)
			}

			comment := refusal.Comment()
			if !strings.Contains(comment, "Detent refused to dispatch this issue") {
				t.Fatalf("Refusal.Comment() = %q, want refusal explanation", comment)
			}
			if !strings.Contains(comment, FormatUSD(tt.wantSpend)) {
				t.Fatalf("Refusal.Comment() = %q, want projected spend %s", comment, FormatUSD(tt.wantSpend))
			}
			if tt.spend != nil {
				assertCallCounts(t, tt.spend, tt.wantDaily, tt.wantIssues)
			}
		})
	}
}

func TestCheckerUsesDefaultEstimate(t *testing.T) {
	t.Parallel()

	checker := NewChecker(Config{
		Enabled:      true,
		PerDayMaxUSD: 0.01,
	}, &fakeSpendStore{}, PricingTable{
		"gpt-test": {
			USDPerInputToken:  0.000001,
			USDPerOutputToken: 0.000002,
		},
	})

	decision, err := checker.CheckDispatch(context.Background(), DispatchRequest{
		Model: "gpt-test",
		Now:   time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("CheckDispatch() error = %v", err)
	}
	if decision.Refusal == nil {
		t.Fatal("Decision.Refusal = nil, want default estimate to exceed cap")
	}
	assertInDelta(t, decision.Refusal.ProjectedCostUSD, 0.19)
}

func TestRefusalTrackerRecordsOncePerCooldown(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	tracker := NewRefusalTracker()
	refusal := Refusal{
		Code:          ReasonPerDayMaxUSD,
		IssueID:       "issue-cooldown",
		RefusedAt:     now,
		CooldownUntil: now.Add(time.Hour),
	}

	first, shouldComment := tracker.Record(refusal)
	if !shouldComment {
		t.Fatal("first Record() shouldComment = false, want true")
	}
	if first.DueAt != now.Add(time.Hour) {
		t.Fatalf("first DueAt = %s, want %s", first.DueAt, now.Add(time.Hour))
	}
	if !tracker.CooldownActive("issue-cooldown", now.Add(30*time.Minute)) {
		t.Fatal("CooldownActive() = false, want true")
	}

	refusal.RefusedAt = now.Add(30 * time.Minute)
	again, shouldComment := tracker.Record(refusal)
	if shouldComment {
		t.Fatal("second Record() shouldComment = true, want false")
	}
	if again.Refusal.RefusedAt != now {
		t.Fatalf("second Record() RefusedAt = %s, want original %s", again.Refusal.RefusedAt, now)
	}

	refusal.RefusedAt = now.Add(time.Hour)
	refusal.CooldownUntil = now.Add(2 * time.Hour)
	third, shouldComment := tracker.Record(refusal)
	if !shouldComment {
		t.Fatal("third Record() shouldComment = false, want true after cooldown")
	}
	if third.DueAt != now.Add(2*time.Hour) {
		t.Fatalf("third DueAt = %s, want %s", third.DueAt, now.Add(2*time.Hour))
	}
}

func TestFormatUSD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value float64
		want  string
	}{
		{value: 1, want: "$1.00"},
		{value: 1.235, want: "$1.24"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()

			if got := FormatUSD(tt.value); got != tt.want {
				t.Fatalf("FormatUSD() = %q, want %q", got, tt.want)
			}
		})
	}

	if got := formatOptionalUSD(nil); got != "n/a" {
		t.Fatalf("formatOptionalUSD(nil) = %q, want n/a", got)
	}
}

type fakeSpendStore struct {
	daily        store.TokenSpend
	dailyErr     error
	issues       map[string]store.TokenSpend
	issueErr     error
	dailyCalls   int
	issueCalls   int
	issueLookups []string
}

func (s *fakeSpendStore) DailyTokenSpend(_ context.Context, day time.Time) (store.TokenSpend, error) {
	s.dailyCalls++
	if day.IsZero() {
		return store.TokenSpend{}, errors.New("zero day")
	}
	if s.dailyErr != nil {
		return store.TokenSpend{}, s.dailyErr
	}
	return s.daily, nil
}

func (s *fakeSpendStore) IssueTokenSpend(_ context.Context, identity store.IssueIdentity) (store.TokenSpend, error) {
	s.issueCalls++
	s.issueLookups = append(s.issueLookups, issueKey(identity))
	if s.issueErr != nil {
		return store.TokenSpend{}, s.issueErr
	}
	if s.issues == nil {
		return store.TokenSpend{}, nil
	}
	return s.issues[issueKey(identity)], nil
}

func assertCallCounts(t *testing.T, spend *fakeSpendStore, wantDaily int, wantIssues int) {
	t.Helper()

	if spend.dailyCalls != wantDaily {
		t.Fatalf("DailyTokenSpend calls = %d, want %d", spend.dailyCalls, wantDaily)
	}
	if spend.issueCalls != wantIssues {
		t.Fatalf("IssueTokenSpend calls = %d, want %d", spend.issueCalls, wantIssues)
	}
}

func assertInDelta(t *testing.T, got float64, want float64) {
	t.Helper()

	const tolerance = 0.000001
	delta := got - want
	if delta < 0 {
		delta = -delta
	}
	if delta > tolerance {
		t.Fatalf("value = %.12f, want %.12f", got, want)
	}
}

func issueKey(identity store.IssueIdentity) string {
	return identity.IssueID + "|" + identity.Identifier + "|" + identity.IssueURL
}
