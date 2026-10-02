// Package operatoradmin connects typed administration tools to the existing
// application commands and bounded chat approval sessions. It owns no storage.
package operatoradmin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

var ErrUnavailable = errors.New("administration operation is unavailable")

// Input is an application input, decoded against each individual tool schema.
// Authority and confirmation mode never appear here.
type Input struct {
	Grants         []ProjectGrant `json:"grants,omitzero"`
	RequestID      string         `json:"request_id,omitempty"`
	Offset         int            `json:"offset,omitempty"`
	Limit          int            `json:"limit,omitempty"`
	OrganizationID string         `json:"organization_id,omitempty"`
	Name           string         `json:"name,omitempty"`
	ConfirmName    string         `json:"confirm_name,omitempty"`
	InvitationID   string         `json:"invitation_id,omitempty"`
	MemberID       string         `json:"member_id,omitempty"`
	Email          string         `json:"email,omitempty"`
	Role           string         `json:"role,omitempty"`
	ProjectID      string         `json:"project_id,omitempty"`
	Write          bool           `json:"write,omitempty"`
	Runner         bool           `json:"runner,omitempty"`
	Revoke         bool           `json:"revoke,omitempty"`
	CredentialID   string         `json:"credential_id,omitempty"`
	Scopes         []string       `json:"scopes,omitempty"`
	ProjectAccess  string         `json:"project_access,omitempty"`
	ProjectIDs     []string       `json:"project_ids,omitempty"`
	ExpiresIn      string         `json:"expires_in,omitempty"`
	Grace          string         `json:"grace,omitempty"`
	Reason         string         `json:"reason,omitempty"`
}

type ProjectGrant struct {
	ProjectID string `json:"project_id"`
	Write     bool   `json:"write"`
	Runner    bool   `json:"runner"`
}

type Preview struct {
	ResourceID string
	Summary    string
	Current    any // Safe, exact target state; never credentials or provider payloads.
}

type Output struct {
	SignOut    *operatortool.SignOutResult `json:"sign_out,omitempty"`
	ResourceID string                      `json:"resource_id,omitempty"`
	URL        string                      `json:"url,omitempty"`
	Reconnect  bool                        `json:"reconnect,omitempty"`
	Data       json.RawMessage             `json:"data,omitempty"`
}

// Application reuses the deployment's dashboard services, ownership checks,
// durable idempotency records and audit sink. Execute must omit secrets from
// durable receipts, and Authorize must run even before a replay is returned.
type Application interface {
	Authorize(context.Context, string, Input, string) error
	Read(context.Context, string, Input) (any, error)
	Preview(context.Context, string, Input) (Preview, error)
	Execute(context.Context, string, Input, mutation.Metadata) (Output, error)
	Audit(context.Context, mutation.Metadata, string)
}

type outputAuthorizer interface {
	AuthorizeOutput(context.Context, string, Input, Output) error
}

type Executor struct {
	App   Application
	Names []string
	Chat  *chat.Service
}

func New(app Application, names ...string) *Executor {
	e := &Executor{App: app, Names: names}
	e.Chat = chat.NewService(nil, nil, e)
	return e
}

func (e *Executor) supports(name string) bool {
	for _, n := range e.Names {
		if n == name {
			return true
		}
	}
	return false
}

func (e *Executor) OpenConnection(ctx context.Context) error {
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: operatortool.AdministrationScope(operatortool.OrganizationSession)}); err != nil {
		return err
	}
	return e.Chat.AttachConnection(ctx)
}

func (e *Executor) ListTools(ctx context.Context) ([]operatortool.Definition, error) {
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: operatortool.AdministrationScope(operatortool.OrganizationSession)}); err != nil {
		return nil, err
	}
	var defs []operatortool.Definition
	for _, d := range operatortool.AdministrationCatalog() {
		if !e.supports(d.Name) {
			continue
		}
		if err := e.authorize(ctx, d.Name, Input{}, ""); err == nil {
			defs = append(defs, d)
		}
	}
	for _, d := range operatortool.CommandCatalog() {
		if d.Name == operatortool.ActionResult || d.Name == operatortool.ConnectionInfo {
			defs = append(defs, d)
		}
	}
	return defs, nil
}

func (e *Executor) authorize(ctx context.Context, name string, in Input, resource string) error {
	if e.App == nil || !e.supports(name) {
		return ErrUnavailable
	}
	requirement := operatortool.AdministrationRequirement(name)
	requirement.ProjectID = in.ProjectID
	if _, err := operatortool.AuthorizeCurrent(ctx, requirement); err != nil {
		return err
	}
	return safe(e.App.Authorize(ctx, name, in, resource))
}

func (e *Executor) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	if call.Name == operatortool.ConnectionInfo {
		if err := operatortool.DecodeArguments(call.Arguments, &struct{}{}); err != nil {
			return operatortool.Result{}, err
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: operatortool.AdministrationScope(operatortool.OrganizationSession)}); err != nil {
			return operatortool.Result{}, err
		}
		if err := e.Chat.CheckConnection(ctx); err != nil {
			return operatortool.Result{}, err
		}
		c := operatortool.CurrentConnection(ctx)
		return result(struct {
			ID           string              `json:"connection_id"`
			Organization string              `json:"organization_id"`
			Mode         chat.ConnectionMode `json:"mode"`
			URL          string              `json:"setup_url"`
		}{c.ID, c.Identity.OrganizationID, e.Chat.Conversation(c.ID).Mode, approvalURL(ctx)})
	}
	if call.Name == operatortool.ActionResult {
		var req struct {
			ActionID string `json:"action_id"`
		}
		if err := operatortool.DecodeArguments(call.Arguments, &req); err != nil || req.ActionID == "" || len(req.ActionID) > 256 {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		if err := e.Chat.CheckConnection(ctx); err != nil {
			return operatortool.Result{}, err
		}
		action, ok := e.Chat.Action(operatortool.CurrentConnection(ctx).ID, req.ActionID)
		if !ok {
			return operatortool.Result{}, ErrUnavailable
		}
		return e.actionResult(ctx, action)
	}
	if !e.supports(call.Name) {
		return operatortool.Result{}, ErrUnavailable
	}
	in, err := Decode(call.Name, call.Arguments)
	if err != nil {
		return operatortool.Result{}, err
	}
	if err := e.authorize(ctx, call.Name, in, ""); err != nil {
		return operatortool.Result{}, err
	}
	d, _ := operatortool.Lookup(call.Name)
	if d.Annotations.ReadOnly {
		data, err := e.App.Read(ctx, call.Name, in)
		if err != nil {
			return operatortool.Result{}, safe(err)
		}
		return result(struct {
			Organization string    `json:"organization_id"`
			FreshAt      time.Time `json:"fresh_at"`
			Data         any       `json:"data"`
		}{operatortool.ConnectionIdentity(ctx).OrganizationID, time.Now().UTC(), data})
	}
	if err := e.Chat.CheckConnection(ctx); err != nil {
		return operatortool.Result{}, err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return operatortool.Result{}, ErrUnavailable
	}
	if action, found, err := e.Chat.RetryResult(ctx, chat.ActionKind(call.Name), in.RequestID, raw); err != nil {
		return operatortool.Result{}, safe(err)
	} else if found {
		return e.actionResult(ctx, action)
	}
	preview, err := e.App.Preview(ctx, call.Name, in)
	if err != nil {
		return operatortool.Result{}, safe(err)
	}
	current, err := json.Marshal(preview.Current)
	if err != nil || len(current) > 32768 {
		return operatortool.Result{}, ErrUnavailable
	}
	hash := sha256.Sum256(current)
	var correlation [16]byte
	if _, err := rand.Read(correlation[:]); err != nil {
		return operatortool.Result{}, ErrUnavailable
	}
	identity := operatortool.ConnectionIdentity(ctx)
	m := mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: in.ProjectID, ResourceID: preview.ResourceID, Action: call.Name, Source: "mcp", CorrelationID: hex.EncodeToString(correlation[:])}
	m, err = m.Bind(in.RequestID, json.RawMessage(raw))
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	action, err := e.Chat.Submit(ctx, chat.Action{Kind: chat.ActionKind(call.Name), RequestID: in.RequestID, Arguments: raw, ProjectID: in.ProjectID, IssueID: preview.ResourceID, Title: preview.Summary, Description: string(current), CurrentState: hex.EncodeToString(hash[:]), Mutation: m})
	if err != nil {
		return operatortool.Result{}, safe(err)
	}
	// Only the just-executed sign-out can return its content-free outcome after
	// ending its own authority. Cached reads/retries still require current access.
	if call.Name == operatortool.SessionLogout && action.Status == chat.ActionSucceeded && action.SignOut != nil {
		return result(struct {
			Action chat.Action `json:"action"`
		}{action})
	}
	return e.actionResult(ctx, action)
}

func (e *Executor) ExecuteAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	name := string(action.Kind)
	in, err := Decode(name, action.Arguments)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	if err := e.authorize(ctx, name, in, action.IssueID); err != nil {
		return chat.ActionExecution{}, err
	}
	m := action.Mutation
	identity := operatortool.ConnectionIdentity(ctx)
	bound, err := m.Bind(in.RequestID, action.Arguments)
	if err != nil || m.Source != "mcp" || m.PrincipalID != identity.PrincipalID || m.OrganizationID != identity.OrganizationID || m.Action != name || m.ProjectID != in.ProjectID || m.RetryIdentity != bound.RetryIdentity || m.InputHash != bound.InputHash {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	preview, err := e.App.Preview(ctx, name, in)
	if err != nil {
		return chat.ActionExecution{}, safe(err)
	}
	raw, err := json.Marshal(preview.Current)
	hash := sha256.Sum256(raw)
	if err != nil || preview.ResourceID != action.IssueID || hex.EncodeToString(hash[:]) != action.CurrentState {
		return chat.ActionExecution{}, ErrUnavailable
	}
	m.Mode, m.Confirmation = string(action.Mode), action.Mutation.Confirmation
	output, err := e.App.Execute(mutation.WithContext(ctx, m), name, in, m)
	if err != nil {
		return chat.ActionExecution{}, safe(err)
	}
	data, err := json.Marshal(output)
	if err != nil || len(data) > operatortool.MaxResultBytes/2 {
		return chat.ActionExecution{}, ErrUnavailable
	}
	execution := chat.ActionExecution{Message: "Administration action completed.", ResourceID: output.ResourceID, URL: output.URL, Data: data}
	if name == operatortool.SessionLogout && output.SignOut != nil {
		execution.SignOut = output.SignOut
		execution.Message = output.SignOut.Message()
	}
	return execution, nil
}

func (e *Executor) AuditAction(ctx context.Context, action chat.Action, outcome string) {
	e.App.Audit(ctx, action.Mutation, outcome)
}

func (e *Executor) actionResult(ctx context.Context, action chat.Action) (operatortool.Result, error) {
	in, err := Decode(string(action.Kind), action.Arguments)
	if err != nil {
		return operatortool.Result{}, err
	}
	if err := e.authorize(ctx, string(action.Kind), in, action.IssueID); err != nil {
		return operatortool.Result{}, err
	}
	data, err := e.Chat.ConnectionResult(ctx, action.ID)
	if err != nil {
		return operatortool.Result{}, safe(err)
	}
	if len(data) > 0 {
		var output Output
		if json.Unmarshal(data, &output) != nil {
			return operatortool.Result{}, ErrUnavailable
		}
		if app, ok := e.App.(outputAuthorizer); ok {
			if err := app.AuthorizeOutput(ctx, string(action.Kind), in, output); err != nil {
				return operatortool.Result{}, safe(err)
			}
		}
	}
	return result(struct {
		Action      chat.Action     `json:"action"`
		ApprovalURL string          `json:"approval_url"`
		ResultTool  string          `json:"result_tool"`
		FreshAt     time.Time       `json:"fresh_at"`
		Output      json.RawMessage `json:"output,omitempty"`
	}{action, action.PendingApprovalURL(approvalURL(ctx)), operatortool.ActionResult, time.Now().UTC(), data})
}

func approvalURL(ctx context.Context) string {
	c := operatortool.CurrentConnection(ctx)
	return strings.TrimRight(c.DashboardURL, "/") + "/chat/approval?connection_id=" + c.ID
}
func result(value any) (operatortool.Result, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > operatortool.MaxResultBytes {
		return operatortool.Result{}, ErrUnavailable
	}
	return operatortool.Result{Content: raw}, nil
}
func safe(err error) error {
	for _, allowed := range []error{operatortool.ErrAccessDenied, operatortool.ErrInvalidArguments, mutation.ErrConflict, mutation.ErrUncertain} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	if err != nil {
		return ErrUnavailable
	}
	return nil
}
