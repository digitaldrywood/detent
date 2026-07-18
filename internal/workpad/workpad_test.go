package workpad

import (
	"reflect"
	"strings"
	"testing"
)

func TestSignalFromComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		body          string
		wantOK        bool
		wantInvalid   string
		wantStatus    string
		wantReason    string
		wantBlockers  []string
		wantHuman     string
		wantComment   string
		wantFields    map[string]string
		wantLastBlock bool
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
			name:        "valid gate field",
			body:        "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: complete\nfields:\n  render_status: pending_review\nblockers: []\nhuman_action: null\n```",
			wantOK:      true,
			wantStatus:  StatusComplete,
			wantFields:  map[string]string{"render_status": "pending_review"},
			wantComment: "https://github.test/comment/1",
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
