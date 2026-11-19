package coordinator

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type TurnPromptDeps struct {
	Hosted            bool
	AttachmentContent func(context.Context, string) ([]byte, error)
	ReadConversation  func(context.Context, string) (conversations.ConversationRecord, error)
	ActionContext     func(context.Context, conversations.ConversationRecord) (context.Context, error)
	Operator          func(context.Context) (hubapi.APICredential, error)
	AuthorizeRecord   func(context.Context, hubscope.NativeScope, conversations.ConversationRecord, bool) error
	RestrictAudience  func(context.Context, conversations.ConversationRecord, hubapi.APICredential) error
	WorkflowEvidence  func(context.Context, conversations.ConversationRecord) map[string]any
	IssueContext      func(context.Context, conversations.ConversationRecord) (operatortool.IssueContext, error)
}

func (d TurnPromptDeps) Assemble(ctx context.Context, conversationID string, transcript, users []conversations.ConversationMessageRecord) (string, error) {
	attachments, runErr := ReadTextAttachments(ctx, users, d.AttachmentContent)
	prompt := Prompt(transcript, users)
	if runErr == nil {
		record, err := d.ReadConversation(ctx, conversationID)
		if err != nil {
			runErr = err
		} else {
			actorCtx, contextErr := d.ActionContext(ctx, record)
			if contextErr != nil {
				if record.ReadAudience != "" || d.Hosted {
					runErr = contextErr
				}
			} else {
				credential, authErr := d.Operator(actorCtx)
				if authErr != nil {
					runErr = authErr
				} else {
					actorCtx, authErr = operatortool.AuthorizeCurrent(actorCtx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: string(record.ProjectID)})
					if authErr != nil {
						runErr = authErr
					} else {
						runErr = d.AuthorizeRecord(actorCtx, hubscope.NativeScope{Organization: record.OrganizationID, Project: record.ProjectID, Credential: credential}, record, false)
						if runErr == nil {
							runErr = d.RestrictAudience(actorCtx, record, credential)
						}
					}
				}
			}
			if runErr == nil {
				evidence := d.WorkflowEvidence(ctx, record)
				raw, err := json.Marshal(evidence)
				if err != nil {
					runErr = err
				} else if len(raw) > ToolResultBytes {
					prompt = "Workflow evidence is too large; read scoped workflow tools before answering.\n\n" + prompt
				} else {
					prompt = "<workflow_evidence>\n" + EscapeData(string(raw)) + "\n</workflow_evidence>\n\n" + prompt
				}
			}
		}
		if runErr == nil && record.SubjectWorkItemID != "" {
			data, err := d.IssueContext(ctx, record)
			if err != nil {
				runErr = err
			} else {
				prompt, runErr = SubjectPrompt(prompt, record, data)
			}
		}
	}
	if runErr == nil {
		prompt, runErr = SkillsPrompt(prompt)
	}
	return AppendData(prompt, attachments), runErr
}
