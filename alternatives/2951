package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestIssueProgressCreditPersistsAndAdvances(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "detent.db")
	db := openParkTestStore(t, path)
	identity := IssueIdentity{
		ProjectID:  "detent",
		IssueID:    "issue-2015",
		Identifier: "digitaldrywood/detent#2015",
		IssueURL:   "https://github.com/digitaldrywood/detent/issues/2015",
	}
	first := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)

	if _, err := db.CreditIssueProgress(t.Context(), identity, first); err != nil {
		t.Fatalf("CreditIssueProgress() error = %v", err)
	}
	credit, err := db.IssueProgressCredit(t.Context(), IssueIdentity{ProjectID: identity.ProjectID, Identifier: identity.Identifier})
	if err != nil {
		t.Fatalf("IssueProgressCredit() error = %v", err)
	}
	if !credit.CreditedAt.Equal(first) || credit.IssueID != identity.IssueID || credit.IssueURL != identity.IssueURL {
		t.Fatalf("credit = %#v, want first credit with complete identity", credit)
	}
	if _, err := db.CreditIssueProgress(t.Context(), identity, second); err != nil {
		t.Fatalf("CreditIssueProgress() advance error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	db = openParkTestStore(t, path)
	credit, err = db.IssueProgressCredit(t.Context(), IssueIdentity{ProjectID: identity.ProjectID, IssueURL: identity.IssueURL})
	if err != nil {
		t.Fatalf("IssueProgressCredit() after restart error = %v", err)
	}
	if !credit.CreditedAt.Equal(second) {
		t.Fatalf("CreditedAt = %s, want %s", credit.CreditedAt, second)
	}
}

func TestIssueProgressCreditValidatesIdentity(t *testing.T) {
	t.Parallel()

	db := openParkTestStore(t, filepath.Join(t.TempDir(), "detent.db"))
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		identity IssueIdentity
		at       time.Time
		want     error
	}{
		{name: "missing project", identity: IssueIdentity{IssueID: "issue-2015"}, at: now, want: ErrProjectRequired},
		{name: "missing issue identity", identity: IssueIdentity{ProjectID: "detent"}, at: now},
		{name: "missing timestamp", identity: IssueIdentity{ProjectID: "detent", IssueID: "issue-2015"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := db.CreditIssueProgress(t.Context(), tt.identity, tt.at)
			if err == nil {
				t.Fatal("CreditIssueProgress() error = nil")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("CreditIssueProgress() error = %v, want %v", err, tt.want)
			}
		})
	}
}
