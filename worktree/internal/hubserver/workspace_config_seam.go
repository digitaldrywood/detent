package hubserver

import "github.com/digitaldrywood/detent/internal/hubserver/workspacehub"

type (
	WorkspaceConfig         = workspacehub.Config
	WorkspaceTerminalConfig = workspacehub.TerminalConfig
)

const (
	defaultWorkspaceIdleTimeout    = workspacehub.DefaultWorkspaceIdleTimeout
	defaultWorkspaceRetainAfterRun = workspacehub.DefaultWorkspaceRetainAfterRun
)

var ErrWorkspaceConfig = workspacehub.ErrWorkspaceConfig
