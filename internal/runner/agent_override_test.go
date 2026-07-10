package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestResolveAgentOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		issue             connector.Issue
		catalogErr        error
		defaultModel      string
		defaultModelErr   error
		baseModel         string
		role              string
		projectEffort     agentEffortCandidate
		wantModel         string
		wantEffort        string
		wantRejectedField string
		wantReason        string
		wantCatalogCalls  int
		wantDefaultCalls  int
	}{
		{
			name:      "no block",
			issue:     connector.Issue{Description: "Ship it."},
			baseModel: "gpt-default",
			wantModel: "gpt-default",
		},
		{
			name:             "model only",
			issue:            connector.Issue{Description: "```detent-agent\nschema: 1\nmodel: gpt-5.5\n```"},
			baseModel:        "gpt-default",
			wantModel:        "gpt-5.5",
			wantCatalogCalls: 1,
		},
		{
			name:             "effort only",
			issue:            connector.Issue{Description: "```detent-agent\nschema: 1\neffort: high\n```"},
			baseModel:        "gpt-default",
			wantModel:        "gpt-default",
			wantEffort:       "high",
			wantCatalogCalls: 1,
		},
		{
			name:             "model and effort",
			issue:            connector.Issue{Description: "```detent-agent\nschema: 1\nmodel: gpt-5.5\neffort: medium\n```"},
			baseModel:        "gpt-default",
			wantModel:        "gpt-5.5",
			wantEffort:       "medium",
			wantCatalogCalls: 1,
		},
		{
			name:             "issue role effort wins",
			issue:            connector.Issue{Description: "```detent-agent\nschema: 1\neffort: high\nmerge:\n  effort: low\n```"},
			baseModel:        "gpt-default",
			role:             RoleMerge,
			projectEffort:    agentEffortCandidate{Field: "agent.effort.merge", Effort: "high"},
			wantModel:        "gpt-default",
			wantEffort:       "low",
			wantCatalogCalls: 1,
		},
		{
			name:             "issue effort wins over project role effort",
			issue:            connector.Issue{Description: "```detent-agent\nschema: 1\neffort: high\n```"},
			baseModel:        "gpt-default",
			role:             RoleMerge,
			projectEffort:    agentEffortCandidate{Field: "agent.effort.merge", Effort: "low"},
			wantModel:        "gpt-default",
			wantEffort:       "high",
			wantCatalogCalls: 1,
		},
		{
			name:             "project role effort wins over backend default",
			issue:            connector.Issue{Description: "Ship it."},
			baseModel:        "gpt-default",
			role:             RoleMerge,
			projectEffort:    agentEffortCandidate{Field: "agent.effort.merge", Effort: "low"},
			wantModel:        "gpt-default",
			wantEffort:       "low",
			wantCatalogCalls: 1,
		},
		{
			name:              "invalid role effort falls back to issue effort",
			issue:             connector.Issue{Description: "```detent-agent\nschema: 1\neffort: high\nmerge:\n  effort: impossible\n```"},
			baseModel:         "gpt-default",
			role:              RoleMerge,
			wantModel:         "gpt-default",
			wantEffort:        "high",
			wantRejectedField: "merge.effort",
			wantCatalogCalls:  1,
		},
		{
			name:              "invalid model",
			issue:             connector.Issue{Description: "```detent-agent\nschema: 1\nmodel: retired\n```"},
			baseModel:         "gpt-default",
			wantModel:         "gpt-default",
			wantRejectedField: "model",
			wantCatalogCalls:  1,
		},
		{
			name:              "invalid effort",
			issue:             connector.Issue{Description: "```detent-agent\nschema: 1\neffort: impossible\n```"},
			baseModel:         "gpt-default",
			wantModel:         "gpt-default",
			wantRejectedField: "effort",
			wantCatalogCalls:  1,
		},
		{
			name:              "retired model",
			issue:             connector.Issue{Description: "```detent-agent\nschema: 1\nmodel: gpt-retired\n```"},
			baseModel:         "gpt-default",
			wantModel:         "gpt-default",
			wantRejectedField: "model",
			wantReason:        `retired; use "gpt-5.5"`,
			wantCatalogCalls:  1,
		},
		{
			name: "block in comment ignored",
			issue: connector.Issue{Comments: []connector.IssueComment{{
				Body: "```detent-agent\nschema: 1\nmodel: gpt-5.5\neffort: medium\n```",
			}}},
			baseModel: "gpt-default",
			wantModel: "gpt-default",
		},
		{
			name:              "catalog failure falls back",
			issue:             connector.Issue{Description: "```detent-agent\nschema: 1\nmodel: gpt-5.5\n```"},
			catalogErr:        errors.New("offline"),
			baseModel:         "gpt-default",
			wantModel:         "gpt-default",
			wantRejectedField: "model",
			wantCatalogCalls:  1,
		},
		{
			name:             "effort only uses operator configured model",
			issue:            connector.Issue{Description: "```detent-agent\nschema: 1\neffort: medium\n```"},
			defaultModel:     "gpt-5.5",
			wantEffort:       "medium",
			wantCatalogCalls: 1,
			wantDefaultCalls: 1,
		},
		{
			name:              "default model lookup failure rejects effort",
			issue:             connector.Issue{Description: "```detent-agent\nschema: 1\neffort: medium\n```"},
			defaultModelErr:   errors.New("config unavailable"),
			wantRejectedField: "effort",
			wantCatalogCalls:  1,
			wantDefaultCalls:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend := &catalogAgentBackend{
				models: []AgentModel{
					{ID: "gpt-default", Model: "gpt-default", Default: true, SupportedReasoningEfforts: []string{"low", "high"}},
					{ID: "gpt-5.5", Model: "gpt-5.5", SupportedReasoningEfforts: []string{"low", "medium", "high"}},
					{ID: "gpt-retired", Model: "gpt-retired", Upgrade: "gpt-5.5", SupportedReasoningEfforts: []string{"medium"}},
				},
				err:             tt.catalogErr,
				defaultModel:    tt.defaultModel,
				defaultModelErr: tt.defaultModelErr,
			}
			role := tt.role
			if role == "" {
				role = RoleCode
			}
			got := resolveAgentOverride(context.Background(), tt.issue, "/tmp/workspace", tt.baseModel, role, tt.projectEffort, backend)
			if got.Model != tt.wantModel || got.Effort != tt.wantEffort {
				t.Fatalf("resolved override = %#v, want model %q effort %q", got, tt.wantModel, tt.wantEffort)
			}
			if backend.calls != tt.wantCatalogCalls {
				t.Fatalf("catalog calls = %d, want %d", backend.calls, tt.wantCatalogCalls)
			}
			if backend.defaultCalls != tt.wantDefaultCalls {
				t.Fatalf("default model calls = %d, want %d", backend.defaultCalls, tt.wantDefaultCalls)
			}
			if tt.wantRejectedField == "" {
				if len(got.Rejections) != 0 {
					t.Fatalf("rejections = %#v, want none", got.Rejections)
				}
				return
			}
			if len(got.Rejections) != 1 || got.Rejections[0].Field != tt.wantRejectedField {
				t.Fatalf("rejections = %#v, want field %q", got.Rejections, tt.wantRejectedField)
			}
			if tt.wantReason != "" && !strings.Contains(got.Rejections[0].Reason, tt.wantReason) {
				t.Fatalf("rejection reason = %q, want containing %q", got.Rejections[0].Reason, tt.wantReason)
			}
		})
	}
}

type catalogAgentBackend struct {
	models          []AgentModel
	err             error
	calls           int
	defaultModel    string
	defaultModelErr error
	defaultCalls    int
}

func (*catalogAgentBackend) RunTurn(context.Context, AgentTurnRequest, AgentUpdateHandler) (AgentTurnResult, error) {
	return AgentTurnResult{}, nil
}

func (b *catalogAgentBackend) ListModels(context.Context) ([]AgentModel, error) {
	b.calls++
	return b.models, b.err
}

func (b *catalogAgentBackend) DefaultModel(_ context.Context, workspace string) (string, error) {
	b.defaultCalls++
	if workspace != "/tmp/workspace" {
		return "", errors.New("unexpected workspace")
	}
	return b.defaultModel, b.defaultModelErr
}
