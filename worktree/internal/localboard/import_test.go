package localboard

import "testing"

func TestSourceKeyHistoricalVectors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, store, project, kind, want string
	}{
		{"historical key", "example-board", "default", "local_sqlite", "53f0548dab155ec779ace50d0578b1980e46ec7df8ca72f1789cab55f2e48e0d"},
		{"other project", "example-board", "other", "local_sqlite", "83aa62e1be9e6ec99b757ec78640b4769dc82c33d6ecfe8f47f2f20672ef0e3b"},
		{"other store", "other-board", "default", "local_sqlite", "c8ddb1a5ff8417defa394b89272329ef4d6725fcd231f5beb176454898f58d55"},
		{"other source kind", "example-board", "default", "github_local", "268c15b51443a18b7afafe67210c10659ce40d10d2a2323bac90012b87d49f65"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := SourceKey(test.store, test.project, test.kind, "z-first"); got != test.want {
				t.Fatalf("source key = %s, want %s", got, test.want)
			}
		})
	}
}
