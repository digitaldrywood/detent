package agentoverride

import (
	"strings"
	"testing"
)

func TestFromIssueBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		want      Override
		wantFound bool
		wantErr   string
	}{
		{name: "no block", body: "Ship the feature."},
		{
			name:      "model only",
			body:      "```detent-agent\nschema: 1\nmodel: gpt-5.5\n```",
			want:      Override{Model: "gpt-5.5"},
			wantFound: true,
		},
		{
			name:      "effort only",
			body:      "```detent-agent\nschema: 1\neffort: medium\n```",
			want:      Override{Effort: "medium"},
			wantFound: true,
		},
		{
			name:      "model and effort",
			body:      "~~~detent-agent\nschema: 1\nmodel: gpt-5.5\neffort: high\n~~~",
			want:      Override{Model: "gpt-5.5", Effort: "high"},
			wantFound: true,
		},
		{
			name:      "last block wins",
			body:      "```detent-agent\nschema: 1\nmodel: old\n```\n\n```detent-agent\nschema: 1\nmodel: current\n```",
			want:      Override{Model: "current"},
			wantFound: true,
		},
		{
			name:      "unknown field",
			body:      "```detent-agent\nschema: 1\nmodel: gpt-5.5\nextra: nope\n```",
			wantFound: true,
			wantErr:   "field extra not found",
		},
		{
			name:      "unsupported schema",
			body:      "```detent-agent\nschema: 2\nmodel: gpt-5.5\n```",
			wantFound: true,
			wantErr:   "schema must be 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, found, err := FromIssueBody(tt.body)
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromIssueBody() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("override = %#v, want %#v", got, tt.want)
			}
		})
	}
}
