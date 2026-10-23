package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestValidatorContextStorageRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	cfg := Config{Backend: BackendSQLite, Path: filepath.Join(t.TempDir(), "verdicts.db")}
	first, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	for _, tt := range []struct {
		context, repo, base, head string
		pr                        int64
	}{
		{"A", "o/r", "base", "head", 207}, {"B", "o/r", "base", "head", 207},
		{"B", "o/r", "base", "head", 208}, {"B", "fork/r", "base", "head", 207},
		{"B", "o/r", "other-base", "head", 207}, {"B", "o/r", "base", "other-head", 207},
	} {
		if err := first.RecordValidatorVerdict(t.Context(), ValidatorVerdict{ProjectID: "detent", IssueID: "200", ContextDigest: tt.context, Repository: tt.repo, BaseSHA: tt.base, HeadSHA: tt.head, PRNumber: &tt.pr, Submitted: true, Verdict: "pass", DiffDigest: "diff", RecordedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	key := ValidatorVerdictKey{ProjectID: "detent", IssueID: "200", ContextDigest: "B", Repository: "o/r", BaseSHA: "base", HeadSHA: "head", PRNumber: 207}
	if err := reloaded.MarkValidatorVerdictCommented(t.Context(), key, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	records, err := reloaded.ListValidatorVerdicts(t.Context(), ValidatorVerdictQuery{ProjectID: "detent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 6 {
		t.Fatalf("distinct context/provenance rows=%d, want 6", len(records))
	}
	for _, record := range records {
		want := record.ContextDigest == "B" && record.Repository == "o/r" && record.BaseSHA == "base" && record.HeadSHA == "head" && *record.PRNumber == 207
		if record.Commented != want {
			t.Fatalf("comment marker crossed identity: %+v", record)
		}
	}
	verdict, err := reloaded.ValidatorVerdict(t.Context(), key)
	if err != nil || verdict.ContextDigest != "B" {
		t.Fatalf("reload=%+v %v", verdict, err)
	}
	key.ContextDigest = "unknown"
	if _, err := reloaded.ValidatorVerdict(t.Context(), key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown context=%v", err)
	}
}
