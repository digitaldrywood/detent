package orchestrator

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

func TestNativeMergeQueueCandidateGateKinds(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change func(*Config)
		want   bool
	}{
		{"artifact gate", func(c *Config) { c.AutoPromote.Gate.Kind = gate.KindArtifact }, true},
		{"command gate", func(c *Config) { c.AutoPromote.Gate.Kind = gate.KindCommand }, true},
		{"security audit", func(c *Config) { c.AutoPromote.Gate.SecurityAudit.Enabled = true }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging"}})
			tt.change(&cfg)
			issue := nativeMergeQueueTestIssue(2465, "success")
			if got := nativeMergeQueueCandidate(issue, cfg); got != tt.want {
				t.Fatalf("nativeMergeQueueCandidate() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestNativeMergeQueueOwnsIssueRequiresCandidacy(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		available bool
		entry     bool
		change    func(*Config)
		want      bool
	}{
		{"queue available and candidate", true, false, func(*Config) {}, true},
		{"queue available and command gate", true, false, func(c *Config) { c.AutoPromote.Gate.Kind = gate.KindCommand }, true},
		{"queue available but excluded", true, false, func(c *Config) { c.AutoPromote.Gate.SecurityAudit.Enabled = true }, false},
		{"queue unavailable", false, false, func(*Config) {}, false},
		{"existing entry while excluded", false, true, func(c *Config) { c.AutoPromote.Gate.SecurityAudit.Enabled = true }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging"}})
			tt.change(&cfg)
			issue := nativeMergeQueueTestIssue(2465, "success")
			state := newState(cfg)
			state.nativeMergeQueueRepos[nativeMergeQueueRepositoryKey(issue)] = nativeMergeQueueRepository{Available: tt.available, CheckedAt: time.Now()}
			if tt.entry {
				state.nativeMergeQueueEntries[strings.TrimSpace(issue.ID)] = nativeMergeQueueEntry{CheckedAt: time.Now()}
			}
			if got := nativeMergeQueueOwnsIssue(&state, issue, cfg); got != tt.want {
				t.Fatalf("nativeMergeQueueOwnsIssue() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestNativeMergeQueueExcludedLogsOncePerHead(t *testing.T) {
	t.Parallel()
	cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging"}})
	cfg.AutoPromote.Gate.SecurityAudit.Enabled = true
	var logs bytes.Buffer
	orch := &Orchestrator{cfg: cfg, logger: slog.New(slog.NewTextHandler(&logs, nil))}
	state := newState(cfg)
	issue := nativeMergeQueueTestIssue(2465, "success")

	orch.logNativeMergeQueueExcluded(&state, issue)
	orch.logNativeMergeQueueExcluded(&state, issue)
	if got := strings.Count(logs.String(), "merge_worker_native_queue_excluded"); got != 1 {
		t.Fatalf("logged %d times for the same head, want 1: %s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "security_audit=true") {
		t.Fatalf("missing exclusion attributes: %s", logs.String())
	}

	issue.PullRequest.HeadSHA = "0000000000000000000000000000000000000001"
	orch.logNativeMergeQueueExcluded(&state, issue)
	if got := strings.Count(logs.String(), "merge_worker_native_queue_excluded"); got != 2 {
		t.Fatalf("logged %d times after a new head, want 2", got)
	}
}

func TestNativeMergeQueueCandidateUnresolvedReviewThreads(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		kind string
		want bool
	}{
		{"command gate blocks on threads", gate.KindCommand, false},
		{"artifact gate ignores threads", gate.KindArtifact, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := nativeMergeQueueTestConfig(Config{MergeFastPathEnabled: true, ActiveStates: []string{"Merging"}})
			cfg.AutoPromote.Gate.Kind = tt.kind
			issue := nativeMergeQueueTestIssue(2465, "success")
			issue.PullRequest.UnresolvedReviewThreads = []connector.PullRequestReviewThread{{}}
			if got := nativeMergeQueueCandidate(issue, cfg); got != tt.want {
				t.Fatalf("nativeMergeQueueCandidate() = %t, want %t", got, tt.want)
			}
		})
	}
}
