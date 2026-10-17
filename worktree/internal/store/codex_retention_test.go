package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestProtectedCodexThreadIDs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		unfinished, resumed bool
		want                bool
	}{
		{name: "completed"}, {name: "unfinished", unfinished: true, want: true},
		{name: "resume source", resumed: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openParkTestStore(t, filepath.Join(t.TempDir(), "state.db"))
			at := time.Now()
			id, err := db.StartSession(t.Context(), SessionStart{StartedAt: at, ProviderThreadID: "thread"})
			if err != nil {
				t.Fatal(err)
			}
			if !tc.unfinished {
				if err := db.FinishSession(t.Context(), id, SessionFinish{CompletedAt: at, FinalState: "done"}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.resumed {
				if _, err := db.StartSession(t.Context(), SessionStart{StartedAt: at, ProviderThreadID: "resumed", ResumedFromSessionID: id}); err != nil {
					t.Fatal(err)
				}
			}
			ids, err := db.ProtectedCodexThreadIDs(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(ids, "thread"); got != tc.want {
				t.Fatalf("protected=%v, want %v (%v)", got, tc.want, ids)
			}
		})
	}
}
