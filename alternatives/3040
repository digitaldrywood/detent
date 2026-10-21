package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
)

func TestSessionPolicyPinsAndScopesRecovery(t *testing.T) {
	t.Parallel()
	backend := openTestStore(t, t.Context())
	policyStore := backend.(SessionPolicyStore)
	attempts := backend.(WorkAttemptStore)
	descriptor := policy.Descriptor{SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")), Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("stop")), AutomatedReview: "off", MergeMethod: "squash"}}.WithID()
	raw, err := json.Marshal(map[string]policy.Descriptor{"policy": descriptor})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, test := range []struct {
		name, metadata, project  string
		valid, pinned, noAttempt bool
	}{
		{"pinned", string(raw), "one", true, true, false},
		{"wrong project", string(raw), "two", false, false, false},
		{"legacy metadata", "{}", "one", true, false, false},
		{"legacy no attempt", "{}", "one", true, false, true},
		{"invalid descriptor", `{"policy":{}}`, "one", false, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var id int64
			if !test.noAttempt {
				var err error
				id, err = attempts.StartWorkAttempt(t.Context(), WorkAttemptStart{ProjectID: "one", IssueID: test.name, WorkerType: "codex", StartedAt: now, LeaseExpiresAt: now.Add(time.Minute), WorkerMetadataJSON: test.metadata})
				if err != nil {
					t.Fatal(err)
				}
			}
			session, err := backend.StartSession(t.Context(), SessionStart{ProjectID: "one", IssueID: test.name, WorkAttemptID: id, StartedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			got, err := policyStore.SessionPolicy(t.Context(), test.project, session)
			if (err == nil) != test.valid {
				t.Fatalf("SessionPolicy() = %v, valid=%t", err, test.valid)
			}
			if test.pinned && got.ID != descriptor.ID {
				t.Fatalf("session policy = %s, want %s", got.ID, descriptor.ID)
			}
			if test.valid && !test.pinned && got.ID != "" {
				t.Fatalf("legacy session policy = %s, want no pin", got.ID)
			}
		})
	}
}
