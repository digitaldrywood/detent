package workpad

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSignalFromComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		body           string
		wantOK         bool
		wantInvalid    string
		wantStatus     string
		wantReasonCode string
		wantReason     string
		wantBlockers   []string
		wantHuman      string
		wantComment    string
		wantFields     map[string]string
		wantLastBlock  bool
	}{
		{
			name:         "valid block",
			body:         "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: \"#1462\"\n    reason: \"needs migration\"\nhuman_action: null\n```",
			wantOK:       true,
			wantStatus:   StatusBlocked,
			wantReason:   "digitaldrywood/detent#1462: needs migration",
			wantBlockers: []string{"digitaldrywood/detent#1462"},
			wantComment:  "https://github.test/comment/1",
		},
		{
			name:           "valid blocked recovery reason",
			body:           "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: merge-conflict\nblockers: []\nhuman_action: null\n```",
			wantOK:         true,
			wantStatus:     StatusBlocked,
			wantReasonCode: "merge_conflict",
			wantReason:     "merge_conflict",
			wantComment:    "https://github.test/comment/1",
		},
		{
			name:        "valid gate field",
			body:        "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: complete\nfields:\n  render_status: pending_review\nblockers: []\nhuman_action: null\n```",
			wantOK:      true,
			wantStatus:  StatusComplete,
			wantFields:  map[string]string{"render_status": "pending_review"},
			wantComment: "https://github.test/comment/1",
		},
		{
			name:        "reason code requires blocked status",
			body:        "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: complete\nreason_code: merge_conflict\nblockers: []\nhuman_action: null\n```",
			wantOK:      true,
			wantInvalid: "reason_code is only valid when status is blocked",
		},
		{
			name:        "malformed yaml",
			body:        "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: \"#1462\"\n    reason: [unterminated\nhuman_action: null\n```",
			wantOK:      true,
			wantInvalid: "parse detent-status YAML",
		},
		{
			name:        "unknown field",
			body:        "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: null\nextra: nope\n```",
			wantOK:      true,
			wantInvalid: "field extra not found",
		},
		{
			name:        "invalid status value",
			body:        "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: human-review\nblockers: []\nhuman_action: null\n```",
			wantOK:      true,
			wantInvalid: `status "human-review" must be one of in_progress, blocked, complete`,
		},
		{
			name:        "bad ref",
			body:        "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: \"not-a-ref\"\nhuman_action: null\n```",
			wantOK:      true,
			wantInvalid: "blockers[0].ref",
		},
		{
			name:        "empty blockers blocked malformation",
			body:        "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: null\n```",
			wantOK:      true,
			wantInvalid: "status blocked requires",
		},
		{
			name:       "blocked by human action only",
			body:       "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: \"Need owner approval\"\n```",
			wantOK:     true,
			wantStatus: StatusBlocked,
			wantReason: "Need owner approval",
			wantHuman:  "Need owner approval",
		},
		{
			name:          "last block wins",
			body:          "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: \"old\"\n```\n\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```",
			wantOK:        true,
			wantStatus:    StatusComplete,
			wantLastBlock: true,
		},
		{
			name: "no block",
			body: "## Codex Workpad\n\n### Blockers\n- Blocked by: #1462",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := SignalFromComment(tt.body, "https://github.test/comment/1", "digitaldrywood/detent")
			if ok != tt.wantOK {
				t.Fatalf("SignalFromComment() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.CommentURL != tt.wantComment && tt.wantComment != "" {
				t.Fatalf("CommentURL = %q, want %q", got.CommentURL, tt.wantComment)
			}
			if tt.wantInvalid != "" {
				if got.Invalid == nil {
					t.Fatalf("Invalid = nil, want message containing %q", tt.wantInvalid)
				}
				if !strings.Contains(got.Invalid.Message, tt.wantInvalid) {
					t.Fatalf("Invalid.Message = %q, want containing %q", got.Invalid.Message, tt.wantInvalid)
				}
				if got.Invalid.Hash == "" {
					t.Fatal("Invalid.Hash is empty")
				}
				return
			}
			if got.Invalid != nil {
				t.Fatalf("Invalid = %#v, want nil", got.Invalid)
			}
			if got.Status != tt.wantStatus {
				t.Fatalf("Status = %q, want %q", got.Status, tt.wantStatus)
			}
			if got.ReasonCode != tt.wantReasonCode {
				t.Fatalf("ReasonCode = %q, want %q", got.ReasonCode, tt.wantReasonCode)
			}
			if got.HumanAction != tt.wantHuman {
				t.Fatalf("HumanAction = %q, want %q", got.HumanAction, tt.wantHuman)
			}
			if !reflect.DeepEqual(got.Fields, tt.wantFields) {
				t.Fatalf("Fields = %#v, want %#v", got.Fields, tt.wantFields)
			}
			if Reason(got) != tt.wantReason {
				t.Fatalf("Reason() = %q, want %q", Reason(got), tt.wantReason)
			}
			if len(got.Blockers) != len(tt.wantBlockers) {
				t.Fatalf("Blockers len = %d, want %d", len(got.Blockers), len(tt.wantBlockers))
			}
			for index, want := range tt.wantBlockers {
				if got.Blockers[index].Identifier != want {
					t.Fatalf("Blockers[%d].Identifier = %q, want %q", index, got.Blockers[index].Identifier, want)
				}
			}
			if tt.wantLastBlock && len(got.Blockers) != 0 {
				t.Fatalf("Blockers = %#v, want none from last block", got.Blockers)
			}
		})
	}
}

func TestCurrentAttemptCompletion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		signal     *Signal
		attemptID  int64
		generation uint64
		want       bool
	}{
		{
			name: "current structured completion",
			signal: &Signal{
				Source: SourceStructured,
				Status: StatusComplete,
				Fields: map[string]string{
					FieldCompletionAttempt:    "3295",
					FieldCompletionGeneration: "7",
				},
			},
			attemptID:  3295,
			generation: 7,
			want:       true,
		},
		{
			name: "stale attempt",
			signal: &Signal{
				Source: SourceStructured,
				Status: StatusComplete,
				Fields: map[string]string{
					FieldCompletionAttempt:    "3238",
					FieldCompletionGeneration: "6",
				},
			},
			attemptID:  3295,
			generation: 7,
		},
		{
			name: "operator move without handshake",
			signal: &Signal{
				Source: SourceStructured,
				Status: StatusComplete,
			},
			attemptID:  3295,
			generation: 7,
		},
		{
			name: "blocked completion is not accepted",
			signal: &Signal{
				Source:   SourceStructured,
				Status:   StatusComplete,
				Blockers: []Blocker{{Reason: "waiting"}},
				Fields: map[string]string{
					FieldCompletionAttempt:    "3295",
					FieldCompletionGeneration: "7",
				},
			},
			attemptID:  3295,
			generation: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := CurrentAttemptCompletion(tt.signal, tt.attemptID, tt.generation); got != tt.want {
				t.Fatalf("CurrentAttemptCompletion() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOperationalCompletion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		signal       *Signal
		wantEvidence string
		want         bool
	}{
		{
			name: "structured complete declaration",
			signal: &Signal{
				Source: SourceStructured,
				Status: StatusComplete,
				Fields: map[string]string{
					"completion_kind":     "operational",
					"completion_evidence": "Runner service is healthy and accepting jobs.",
				},
			},
			wantEvidence: "Runner service is healthy and accepting jobs.",
			want:         true,
		},
		{
			name: "missing evidence",
			signal: &Signal{
				Source: SourceStructured,
				Status: StatusComplete,
				Fields: map[string]string{"completion_kind": "operational"},
			},
		},
		{
			name: "ordinary completion",
			signal: &Signal{
				Source: SourceStructured,
				Status: StatusComplete,
			},
		},
		{
			name: "blocked declaration",
			signal: &Signal{
				Source: SourceStructured,
				Status: StatusBlocked,
				Fields: map[string]string{
					"completion_kind":     "operational",
					"completion_evidence": "Runner service is healthy.",
				},
			},
		},
		{
			name: "completion with blocker",
			signal: &Signal{
				Source:   SourceStructured,
				Status:   StatusComplete,
				Blockers: []Blocker{{Reason: "waiting for verification"}},
				Fields: map[string]string{
					"completion_kind":     "operational",
					"completion_evidence": "Runner service is healthy.",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotEvidence, got := OperationalCompletion(tt.signal)
			if got != tt.want {
				t.Fatalf("OperationalCompletion() ok = %t, want %t", got, tt.want)
			}
			if gotEvidence != tt.wantEvidence {
				t.Fatalf("OperationalCompletion() evidence = %q, want %q", gotEvidence, tt.wantEvidence)
			}
		})
	}
}

func TestCompletionAuthorizationFromIssueBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		wantKind  string
		wantFound bool
		wantError string
	}{
		{
			name:      "operational completion authorized",
			body:      "## Completion contract\n\n```detent-completion\nschema: 1\ncompletion_kind: operational\n```",
			wantKind:  CompletionOperational,
			wantFound: true,
		},
		{
			name:      "last authorization wins",
			body:      "```detent-completion\nschema: 1\ncompletion_kind: unsupported\n```\n\n```detent-completion\nschema: 1\ncompletion_kind: operational\n```",
			wantKind:  CompletionOperational,
			wantFound: true,
		},
		{name: "ordinary issue body"},
		{
			name:      "unsupported completion kind",
			body:      "```detent-completion\nschema: 1\ncompletion_kind: repository\n```",
			wantFound: true,
			wantError: `completion_kind "repository" must be operational`,
		},
		{
			name:      "unknown field",
			body:      "```detent-completion\nschema: 1\ncompletion_kind: operational\nextra: nope\n```",
			wantFound: true,
			wantError: "field extra not found",
		},
		{
			name:      "wrong schema",
			body:      "```detent-completion\nschema: 2\ncompletion_kind: operational\n```",
			wantFound: true,
			wantError: "schema must be 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			kind, found, err := CompletionAuthorizationFromIssueBody(tt.body)
			if found != tt.wantFound {
				t.Fatalf("found = %t, want %t", found, tt.wantFound)
			}
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("CompletionAuthorizationFromIssueBody() error = %v", err)
			}
			if kind != tt.wantKind {
				t.Fatalf("kind = %q, want %q", kind, tt.wantKind)
			}
			if got := OperationalCompletionAuthorized(tt.body); got != (tt.wantKind == CompletionOperational) {
				t.Fatalf("OperationalCompletionAuthorized() = %t", got)
			}
		})
	}
}

func TestParseRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ref     string
		repo    string
		want    string
		wantErr bool
	}{
		{name: "local issue", ref: "#42", repo: "digitaldrywood/detent", want: "digitaldrywood/detent#42"},
		{name: "owner repo issue", ref: "digitaldrywood/pyroapex#1462", repo: "digitaldrywood/detent", want: "digitaldrywood/pyroapex#1462"},
		{name: "local issue without repo", ref: "#42", want: "#42"},
		{name: "zero issue rejected", ref: "#0", repo: "digitaldrywood/detent", wantErr: true},
		{name: "url rejected", ref: "https://github.com/digitaldrywood/detent/issues/42", repo: "digitaldrywood/detent", wantErr: true},
		{name: "missing repo rejected", ref: "detent#42", repo: "digitaldrywood/detent", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseRef(tt.ref, tt.repo)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseRef() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ParseRef() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseStatusBlockTypedBlockers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		blocker            string
		wantType           string
		wantOwner          string
		wantIdentifier     string
		wantState          string
		wantCheck          string
		wantScope          string
		wantFingerprint    string
		wantPresent        *bool
		wantUnverifiable   bool
		wantRecheck        string
		wantExpiry         string
		wantErrorSubstring string
	}{
		{
			name:           "issue state",
			blocker:        "ref: '#42'\n    reason: waiting for closure\n    owner: orchestrator\n    predicate:\n      type: issue_state\n      states: [open]\n    recheck_interval: 30s",
			wantType:       PredicateIssueState,
			wantOwner:      BlockerOwnerOrchestrator,
			wantIdentifier: "digitaldrywood/detent#42",
			wantState:      "open",
			wantRecheck:    "30s",
		},
		{
			name:        "pull request state",
			blocker:     "owner: orchestrator\n    predicate:\n      kind: pull-request-state\n      state: open",
			wantType:    PredicatePullRequestState,
			wantOwner:   BlockerOwnerOrchestrator,
			wantState:   "open",
			wantRecheck: "tick",
		},
		{
			name:        "check presence",
			blocker:     "owner: orchestrator\n    predicate:\n      type: check_presence\n      check: Test\n      present: false",
			wantType:    PredicateCheckPresence,
			wantOwner:   BlockerOwnerOrchestrator,
			wantCheck:   "Test",
			wantPresent: boolPointer(false),
			wantRecheck: "tick",
		},
		{
			name:        "budget capacity",
			blocker:     "owner: orchestrator\n    predicate:\n      type: budget_capacity\n      scope: daily-budget\n      condition: exhausted",
			wantType:    PredicateBudgetCapacity,
			wantOwner:   BlockerOwnerOrchestrator,
			wantScope:   "daily_budget",
			wantRecheck: "tick",
		},
		{
			name:            "config fingerprint",
			blocker:         "owner: orchestrator\n    predicate:\n      type: config_fingerprint\n      fingerprint: config-a\n    expires_at: '2026-08-17T12:00:00Z'",
			wantType:        PredicateConfigFingerprint,
			wantOwner:       BlockerOwnerOrchestrator,
			wantFingerprint: "config-a",
			wantRecheck:     "tick",
			wantExpiry:      "2026-08-17T12:00:00Z",
		},
		{
			name:             "free text accepted and flagged",
			blocker:          "reason: waiting for somebody to investigate",
			wantOwner:        BlockerOwnerHuman,
			wantUnverifiable: true,
		},
		{
			name:               "typed predicate validation",
			blocker:            "owner: orchestrator\n    predicate:\n      type: check_presence\n      check: Test",
			wantErrorSubstring: "predicate.present is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			content := "schema: 1\nstatus: blocked\nblockers:\n  - " + tt.blocker + "\nhuman_action: null"
			signal, err := ParseStatusBlock(content, "digitaldrywood/detent")
			if tt.wantErrorSubstring != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrorSubstring) {
					t.Fatalf("ParseStatusBlock() error = %v, want containing %q", err, tt.wantErrorSubstring)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseStatusBlock() error = %v", err)
			}
			if len(signal.Blockers) != 1 {
				t.Fatalf("Blockers = %#v, want one", signal.Blockers)
			}
			blocker := signal.Blockers[0]
			if blocker.Owner != tt.wantOwner || blocker.Unverifiable != tt.wantUnverifiable || blocker.RecheckInterval != tt.wantRecheck {
				t.Fatalf("blocker metadata = %#v", blocker)
			}
			if tt.wantExpiry != "" && (blocker.ExpiresAt == nil || blocker.ExpiresAt.Format(time.RFC3339) != tt.wantExpiry) {
				t.Fatalf("ExpiresAt = %v, want %s", blocker.ExpiresAt, tt.wantExpiry)
			}
			if tt.wantType == "" {
				if blocker.Predicate != nil {
					t.Fatalf("Predicate = %#v, want nil", blocker.Predicate)
				}
				return
			}
			predicate := blocker.Predicate
			if predicate == nil {
				t.Fatal("Predicate = nil")
			}
			if predicate.Type != tt.wantType || predicate.Identifier != tt.wantIdentifier || predicate.Check != tt.wantCheck || predicate.Scope != tt.wantScope || predicate.Fingerprint != tt.wantFingerprint {
				t.Fatalf("Predicate = %#v", predicate)
			}
			if tt.wantState != "" && (len(predicate.States) != 1 || predicate.States[0] != tt.wantState) {
				t.Fatalf("States = %#v, want %q", predicate.States, tt.wantState)
			}
			if tt.wantPresent != nil && (predicate.Present == nil || *predicate.Present != *tt.wantPresent) {
				t.Fatalf("Present = %v, want %t", predicate.Present, *tt.wantPresent)
			}
		})
	}
}

func boolPointer(value bool) *bool {
	return &value
}
