package hubserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

var errHostedContextUnavailable = errors.New("hosted application context is unavailable")

type hostedContextExecutor struct{ service *Service }

type hostedEventRequest struct {
	ProjectID   string `json:"project_id"`
	WorkspaceID string `json:"workspace_id"`
	Cursor      string `json:"cursor"`
}

type hostedEventCursor struct {
	Organization string `json:"organization_id"`
	Project      string `json:"project_id"`
	Workspace    string `json:"workspace_id"`
	Sequence     int64  `json:"sequence"`
	Revision     int64  `json:"workspace_revision"`
}

type hostedEventObservation struct {
	Sequence  int64                     `json:"sequence"`
	Workspace *workspacesession.Session `json:"workspace,omitempty"`
}

type hostedEventResult struct {
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id"`
	ObservedAt     string `json:"observed_at"`
	Freshness      string `json:"freshness"`
	Semantics      string `json:"semantics"`
	Cursor         string `json:"cursor"`
	Changed        bool   `json:"changed"`
	hostedEventObservation
}

func (e hostedContextExecutor) credential(ctx context.Context, name, project string) (context.Context, apiCredential, error) {
	requirement := operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: project}
	if name == operatortool.AppUpdates {
		requirement.ResourceKind = "runners"
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, requirement)
	if err != nil {
		return ctx, apiCredential{}, err
	}
	s := e.service
	if s.config.Hosted == nil || s.database == nil || s.database.db == nil {
		return ctx, apiCredential{}, errHostedContextUnavailable
	}
	credential, err := (hubAdministration{s}).credential(ctx)
	if err != nil || credential.Hosted == nil || credential.HostedRole == "account" || operatorIdentity(credential, s.config.Hosted.OrganizationID) != operatortool.ConnectionIdentity(ctx) {
		return ctx, apiCredential{}, operatortool.ErrAccessDenied
	}
	if name == operatortool.AppUpdates && !s.appAllRunnerGrants(ctx, credential) {
		return ctx, apiCredential{}, operatortool.ErrAccessDenied
	}
	return ctx, credential, nil
}

func (e hostedContextExecutor) listTools(ctx context.Context) []operatortool.Definition {
	definitions := []operatortool.Definition{}
	for _, d := range operatortool.HostedContextCatalog() {
		if _, _, err := e.credential(ctx, d.Name, ""); err == nil {
			definitions = append(definitions, d)
		}
	}
	return definitions
}

func (e hostedContextExecutor) Execute(ctx context.Context, call operatortool.Call) (result operatortool.Result, resultErr error) {
	defer func() {
		if resultErr == nil {
			return
		}
		switch {
		case errors.Is(resultErr, operatortool.ErrAccessDenied):
			resultErr = operatortool.ErrAccessDenied
		case errors.Is(resultErr, operatortool.ErrInvalidArguments):
			resultErr = operatortool.ErrInvalidArguments
		default:
			resultErr = errHostedContextUnavailable
		}
	}()
	var request hostedEventRequest
	if call.Name == operatortool.HostedEvents {
		if err := operatortool.DecodeArguments(call.Arguments, &request); err != nil || request.ProjectID == "" || len(request.ProjectID) > 256 || len(request.WorkspaceID) > 256 || len(request.Cursor) > 2048 {
			return result, operatortool.ErrInvalidArguments
		}
	} else if call.Name == operatortool.AppBootstrapPayload || call.Name == operatortool.AppUpdates {
		if err := operatortool.DecodeArguments(call.Arguments, &struct{}{}); err != nil {
			return result, err
		}
	} else {
		return result, operatortool.ErrUnknownTool
	}
	ctx, credential, err := e.credential(ctx, call.Name, request.ProjectID)
	if err != nil {
		return result, err
	}
	s := e.service
	var payload any
	switch call.Name {
	case operatortool.AppBootstrapPayload:
		session := auth.Session{}
		if credential.SessionHash != "" {
			session, err = s.storedWebSession(ctx, credential.SessionHash, s.config.now())
			if err != nil {
				return result, operatortool.ErrAccessDenied
			}
		} else if credential.HostedKeyScope != "" {
			if err := s.database.db.QueryRowContext(ctx, "SELECT email FROM hosted_members WHERE user_id=? AND active=1", credential.Hosted.Subject).Scan(&session.Email); err != nil {
				return result, err
			}
		}
		bootstrap, err := s.readAppBootstrap(ctx, credential, session)
		if err != nil {
			return result, err
		}
		if len(bootstrap.Projects) > operatortool.MaxItemLimit || len(bootstrap.Organizations) > operatortool.MaxItemLimit || len(bootstrap.Preferences.Models) > operatortool.MaxItemLimit {
			return result, errHostedContextUnavailable
		}
		payload = struct {
			ObservedAt string `json:"observed_at"`
			Freshness  string `json:"freshness"`
			appBootstrap
		}{s.config.now().UTC().Format(time.RFC3339Nano), "current_observation", bootstrap}
	case operatortool.AppUpdates:
		updates, err := s.readAppUpdates(ctx, credential)
		if err != nil {
			return result, err
		}
		if len(updates.Runners) > operatortool.MaxItemLimit {
			return result, errHostedContextUnavailable
		}
		payload = struct {
			ObservedAt string `json:"observed_at"`
			Freshness  string `json:"freshness"`
			appUpdates
		}{s.config.now().UTC().Format(time.RFC3339Nano), "current_observation", updates}
	case operatortool.HostedEvents:
		scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), project: tracker.ProjectID(request.ProjectID), credential: credential}
		observation, err := s.readHostedEventObservation(ctx, scope, request.WorkspaceID, "")
		if err != nil {
			return result, err
		}
		position := hostedEventCursor{Organization: string(scope.organization), Project: request.ProjectID, Workspace: request.WorkspaceID, Sequence: observation.Sequence}
		if observation.Workspace != nil {
			position.Revision = observation.Workspace.Revision
			raw, err := json.Marshal(observation.Workspace)
			raw, err = operatorWorkspaceProjection(raw, false, err)
			var projected workspacesession.Session
			if err != nil || json.Unmarshal(raw, &projected) != nil {
				return result, errHostedContextUnavailable
			}
			observation.Workspace = &projected
		}
		changed := true
		if request.Cursor != "" {
			raw, err := base64.RawURLEncoding.DecodeString(request.Cursor)
			var previous hostedEventCursor
			if err != nil || operatortool.DecodeArguments(raw, &previous) != nil || previous.Organization != position.Organization || previous.Project != position.Project || previous.Workspace != position.Workspace || previous.Sequence < 0 || previous.Revision < 0 {
				return result, operatortool.ErrInvalidArguments
			}
			changed = previous != position
		}
		raw, err := json.Marshal(position)
		if err != nil {
			return result, err
		}
		payload = hostedEventResult{OrganizationID: string(scope.organization), ProjectID: request.ProjectID, ObservedAt: s.config.now().UTC().Format(time.RFC3339Nano), Freshness: "current_observation", Semantics: "current project invalidation sequence and optional workspace revision; no heartbeat or event replay", Cursor: base64.RawURLEncoding.EncodeToString(raw), Changed: changed, hostedEventObservation: observation}
	}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > operatortool.MaxResultBytes {
		return result, errHostedContextUnavailable
	}
	if err := s.hostedAudit(ctx, credential.Hosted, "action", "mcp "+call.Name, request.ProjectID, http.StatusOK); err != nil {
		return result, err
	}
	return operatortool.Result{Content: raw}, nil
}

func (s *Service) readHostedEventObservation(ctx context.Context, scope nativeScope, workspace, item string) (hostedEventObservation, error) {
	observation := hostedEventObservation{}
	if err := s.requireHostedProject(ctx, s.database.db, scope, false); err != nil {
		return observation, operatortool.ErrAccessDenied
	}
	if err := s.database.authorizeNativeProject(ctx, scope); err != nil {
		return observation, operatortool.ErrAccessDenied
	}
	if item != "" {
		if err := s.database.db.QueryRowContext(ctx, "SELECT event_sequence FROM issues WHERE organization_id = ? AND project_id = ? AND native_id = ?", scope.organization, scope.project, item).Scan(&observation.Sequence); err != nil {
			return observation, err
		}
	} else {
		if err := s.database.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(rowid),0) FROM collaboration_events WHERE organization_id = ? AND project_id = ?", scope.organization, scope.project).Scan(&observation.Sequence); err != nil {
			return observation, err
		}
	}
	if workspace != "" {
		if s.workspaces == nil {
			return observation, errHostedContextUnavailable
		}
		raw, err := s.readWorkspace(ctx, scope, workspace)
		if err != nil {
			return observation, err
		}
		if err := json.Unmarshal(raw, &observation.Workspace); err != nil {
			return observation, err
		}
	}
	return observation, nil
}

func (s *Service) appAllRunnerGrants(ctx context.Context, credential apiCredential) bool {
	if credential.Hosted == nil || !s.hostedAllRunnerGrants(ctx, credential) {
		return false
	}
	if credential.HostedKeyScope == "" {
		return true
	}
	condition, args := credential.projectGrantSQL("p.organization_id", "p.id")
	args = append([]any{s.config.Hosted.OrganizationID}, args...)
	var missing int
	err := s.database.db.QueryRowContext(ctx, "SELECT count(*) FROM projects p WHERE p.organization_id = ? AND NOT ("+condition+")", args...).Scan(&missing)
	return err == nil && missing == 0
}
