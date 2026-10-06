package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/update"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// conversationClientFS holds the built client under app/conversation. It is
// a variable so tests can substitute an empty filesystem.
var conversationClientFS fs.FS = detent.StaticFS()

const conversationClientShell = "app/conversation/index.html"

// conversationClientEntry is the bundle whose bytes identify a client build.
// It is the entry chunk the shell loads, so any change to the application the
// hub serves changes it.
const conversationClientEntry = "app/conversation/app.js"

// The paths the application shell never answers: the JSON API, the sign-in
// exchange, webhooks, the static bundle, health and sign-out. Everything else
// is a client route (decisions section 12).
var (
	appReservedNamespaces = []string{"/api", "/app", "/auth", "/webhooks", "/static"}
	appReservedPaths      = []string{"/health", "/invite", "/logout", "/metrics", "/chat/approval"}
)

// appReserved reports whether a path belongs to the hub rather than the
// client. A reserved namespace owns its root and its whole subtree; a reserved
// path owns only itself, so a client route that merely starts with the same
// letters stays a client route.
func appReserved(path string) bool {
	return slices.Contains(appReservedPaths, path) || slices.ContainsFunc(appReservedNamespaces, func(namespace string) bool {
		return path == namespace || strings.HasPrefix(path, namespace+"/")
	})
}

// registerAppRoutes mounts the React application: its bootstrap payload and
// the catch-all shell. The shell is mounted whether or not the conversation
// product is enabled. A single-tenant hub has no hosted session and mounts
// none of this. The hosted pages still own the paths they register, so the
// catch-all serves only the client routes they leave free.
func (s *Service) registerAppRoutes(e *echo.Echo) {
	e.GET("/app/bootstrap", s.appBootstrapPayload)
	e.GET("/chat/bootstrap", s.appBootstrapPayload)
	e.GET("/app/updates", s.appUpdates)
	// Any method, so an unmatched path still answers not found rather than
	// the method-not-allowed a GET-only wildcard would produce.
	e.Any("/*", s.appShell)
}

// appShell serves the client application shell for every client route. The
// client resolves the route itself; an unauthenticated visitor is sent to
// /login, which renders the shell so the React login card can start the
// sign-in exchange.
func (s *Service) appShell(c echo.Context) error {
	path := c.Request().URL.Path
	if !hostedReadRequest(c) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if appReserved(path) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if path != "/login" {
		// The shell itself carries no customer data; every screen reads the
		// JSON API, which enforces membership. A session is enough here, so
		// a staff account can still reach the support screen.
		if _, _, err := s.hostedSession(c.Request().Context(), c); err != nil {
			return c.Redirect(http.StatusSeeOther, s.hostedSignInPath())
		}
	}
	content, err := fs.ReadFile(conversationClientFS, conversationClientShell)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.config.Logger.Error("application client shell could not be read", "error", err)
		}
		return c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Code: "client_unavailable", Message: "The application client is not built on this hub"})
	}
	c.Response().Header().Set("Cache-Control", "no-cache")
	return c.HTMLBlob(http.StatusOK, conversationClientShellFor(content, s.hostedBase(), s.hostedSignInPath()))
}

const conversationClientAssets = "/static/app/conversation/"

func conversationClientShellFor(content []byte, base, signIn string) []byte {
	meta := fmt.Sprintf(`<meta name="detent-base-path" content="%s"><meta name="detent-sign-in-path" content="%s">`, html.EscapeString(base), html.EscapeString(signIn))
	shell := string(content)
	if base != "" {
		shell = strings.ReplaceAll(shell, `"`+conversationClientAssets, `"`+base+conversationClientAssets)
	}
	if head := strings.Index(shell, "<head>"); head >= 0 {
		at := head + len("<head>")
		return []byte(shell[:at] + meta + shell[at:])
	}
	return []byte(meta + shell)
}

// appClientBuild identifies the client build this hub serves. Version is the
// hub binary's own build (the -X main.version the Makefile links in, carried
// through to Config.Version); Build is a content hash of the served bundle, so
// two hubs on the same version but different client bytes are still told
// apart; ServedAt is the instant this hub started serving it, which a restart
// onto a new build moves.
//
// It rides along with the update report as its second line. Detent's update
// question is about the runners, not the browser, but the served identity is
// the one fact a client would need to answer "is this page itself stale", and
// it costs one already-computed struct to publish it beside the answer.
type appClientBuild struct {
	Version  string `json:"version"`
	Build    string `json:"build"`
	ServedAt string `json:"served_at"`
}

// conversationClientIdentity reads the served bundle once and hashes it. It is
// called at startup, so no request pays for it and the answer cannot change
// under a running process: the embedded filesystem is fixed for the life of
// the binary.
//
// A hub with no client built into it reports an empty Build rather than
// failing to open: it serves no client, so there is no client build to be
// behind, and the pill then has nothing to compare and stays idle.
func conversationClientIdentity(fsys fs.FS, version string, at time.Time) appClientBuild {
	identity := appClientBuild{Version: strings.TrimSpace(version), ServedAt: at.UTC().Format(time.RFC3339)}
	// fs.ReadFile reads and closes the entry in one call, so there is no
	// Close whose error the caller would have to decide about: an embedded
	// file that cannot be read has no build to report, and that is the same
	// answer as a hub with no client built into it.
	content, err := fs.ReadFile(fsys, conversationClientEntry)
	if err != nil {
		return identity
	}
	digest := sha256.Sum256(content)
	identity.Build = hex.EncodeToString(digest[:])
	return identity
}

// The update report behind the sidebar footer's pill (Michael, September 12:
// "this should be a check for detent updates for the runners").
//
// Detent's update question is not about the browser: a hosted client is always
// on the build the hub served it. It is about the machines that do the work.
// Every enrolled runner reports the Detent build it is running on each machine
// heartbeat, and the hub is the same binary — so a runner that is not on the
// hub's version is a host somebody has to go and upgrade.

type appUpdateRunner struct {
	ClaimRefusalReason string `json:"claim_refusal_reason"`
	RunnerID           string `json:"runner_id"`
	DisplayName        string `json:"display_name"`
	// Version is what the host's heartbeat last reported, empty for a runner
	// that has never reported one.
	Version string `json:"version"`
	Online  bool   `json:"online"`
	Behind  bool   `json:"behind"`
}

type appUpdates struct {
	MinimumRunnerVersion string `json:"minimum_runner_version"`
	// Current is the version every runner is expected to be on.
	Current string `json:"current"`
	// Source names where Current came from, so a reader is never left guessing
	// what "behind" is behind. It is always "hub": the hub binary's own build.
	// A release feed would be the other answer, and this endpoint deliberately
	// does not consult one — `detent update` reaches GitHub over the network
	// (cmd/detent/update.go), which is not something a session-gated poll on
	// every open client should be doing.
	Source      string            `json:"source"`
	Runners     []appUpdateRunner `json:"runners"`
	BehindCount int               `json:"behind_count"`
	// Client is the build of the bundle this hub serves, carried alongside so
	// a client that wants to know whether the page itself is stale has it
	// without a second request.
	Client appClientBuild `json:"client"`
}

// detentVersion is the hub's own build, as published. A hub with no version
// linked in reports whatever it has ("dev"): saying so is more use to a reader
// than an empty string, and runnerBehind below refuses to compare it anyway.
func detentVersion(version string) string {
	return strings.TrimSpace(version)
}

// runnerBehind reports whether a runner's build is older than the hub's. A
// version that is not a release version on either side is "cannot tell"
// rather than "behind": an unversioned development build must never light up
// an update badge on somebody's fleet.
func runnerBehind(current string, reported string) bool {
	order, err := update.CompareVersions(reported, current)
	return err == nil && order < 0
}

// appUpdates implements GET /app/updates for the footer's pill: which enrolled
// runners are not on the hub's build.
//
// It needs the same authority as the runners list: a member of this
// organization who is not a viewer and manages runners on every project. It
// reads one row per runner. A revoked enrolment is not in the answer: it is
// not a runner anybody is going to upgrade.
func (s *Service) appUpdates(c echo.Context) error {
	credential, status, err := s.hostedCredential(c.Request().Context(), c)
	if err != nil {
		if status == http.StatusForbidden {
			return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "forbidden", Message: "This account has no access to this organization"})
		}
		return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "unauthorized", Message: "A hosted session is required"})
	}
	payload, err := s.readAppUpdates(c.Request().Context(), credential)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, payload)
}

func (s *Service) readAppUpdates(ctx context.Context, credential apiCredential) (appUpdates, error) {
	if credential.Hosted == nil || credential.HostedRole == "viewer" || !s.appAllRunnerGrants(ctx, credential) {
		return appUpdates{}, nativeNotFound()
	}
	current := detentVersion(s.config.Version)
	minimum := minimumRunnerVersion(current)
	payload := appUpdates{MinimumRunnerVersion: minimum, Current: current, Source: "hub", Runners: []appUpdateRunner{}, Client: s.clientBuild}
	rows, err := s.database.db.QueryContext(ctx, `SELECT r.id, r.display_name, m.version, r.last_heartbeat_at
FROM runner_identities r JOIN machines m ON m.id = r.machine_id JOIN api_tokens t ON t.id = r.token_id
WHERE r.organization_id = ? AND t.revoked_at IS NULL ORDER BY r.display_name, r.id`, s.config.Hosted.OrganizationID)
	if err != nil {
		return appUpdates{}, fmt.Errorf("list runner versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	now := s.config.now()
	for rows.Next() {
		var runner appUpdateRunner
		var heartbeat string
		if err := rows.Scan(&runner.RunnerID, &runner.DisplayName, &runner.Version, &heartbeat); err != nil {
			return appUpdates{}, fmt.Errorf("scan runner version: %w", err)
		}
		at, err := parseTimeValue(heartbeat)
		if err != nil {
			return appUpdates{}, fmt.Errorf("read runner heartbeat: %w", err)
		}
		// The same window readRunner calls "offline".
		runner.Online = !now.Before(at) && now.Before(at.Add(runnerauth.HeartbeatTimeout))
		runner.ClaimRefusalReason = runnerClaimRefusal(minimum, runner.Version)
		runner.Behind = runner.ClaimRefusalReason != ""
		if runner.Behind {
			payload.BehindCount++
		}
		payload.Runners = append(payload.Runners, runner)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return appUpdates{}, fmt.Errorf("list runner versions: %w", err)
	}
	return payload, nil
}

// Bootstrap payload (decisions section 12).

type appBootstrapOrganization struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PublicURL string `json:"public_url"`
	Current   bool   `json:"current"`
}

type appBootstrapActor struct {
	PlatformRole     string `json:"platform_role"`
	PrincipalID      string `json:"principal_id"`
	Subject          string `json:"subject"`
	Email            string `json:"email"`
	Role             string `json:"role"`
	CanManage        bool   `json:"can_manage"`
	CanManageRunners bool   `json:"can_manage_runners"`
}

type appBootstrapProject struct {
	ID               string                        `json:"id"`
	Name             string                        `json:"name"`
	Profile          string                        `json:"profile"`
	CanWrite         bool                          `json:"can_write"`
	CanManageRunners bool                          `json:"can_manage_runners"`
	States           []tracker.NativeState         `json:"states"`
	Capabilities     workspacesession.Capabilities `json:"capabilities"`
}

type appBootstrapSupport struct {
	Actor     string `json:"actor"`
	Reason    string `json:"reason"`
	ExpiresAt string `json:"expires_at"`
}

// appBootstrapPlan is the plan summary every member sees. The full usage
// report stays behind GET /plan for owners and admins.
type appBootstrapPlan struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Source       string `json:"source"`
	WindowEndsAt string `json:"window_ends_at"`
}

type appBootstrapCapabilities struct {
	Coordinator bool `json:"coordinator"`
	Attachments bool `json:"attachments"`
}

type appBootstrapFeature struct {
	Conversation bool `json:"conversation"`
	Workspaces   bool `json:"workspaces"`
}

func (s *Service) appBootstrapFeature() appBootstrapFeature {
	return appBootstrapFeature{Conversation: s.conversations != nil, Workspaces: s.workspaces != nil}
}

// appBootstrapChoice is one option of a turn preference picker. Default
// marks what "auto" resolves to for the project; when the hub does not know
// that, "auto" carries the flag itself (decisions section 14).
//
// A model choice carries four more fields, all optional and all absent unless
// an enrolled runner's backend catalog published them: the reasoning ladder
// that model supports, which rung it defaults to, the provider behind it, and
// whether the provider has named a successor. They are what lets the composer
// show a model's own efforts instead of one fixed list for every model, and
// shelve retired models instead of mixing them in.
type appBootstrapChoice struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Default bool   `json:"default"`
	// Efforts is the model's reasoning ladder, least to most. Empty means the
	// fixed vocabulary is all that is known for it.
	Efforts []string `json:"efforts,omitempty"`
	// DefaultEffort is one of Efforts, or empty.
	DefaultEffort string `json:"default_effort,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Legacy        bool   `json:"legacy,omitempty"`
	// BackendDefault marks the model the reporting backend picks when it is
	// given none, which is the one a picker leads with. Default above is a
	// different fact: what "auto" resolves to for this hub.
	BackendDefault bool `json:"backend_default,omitempty"`
}

// appBootstrapPreferences publishes the choices the composer's model, effort
// and access pickers offer. "Auto" is always the first option of each list.
type appBootstrapPreferences struct {
	Models  []appBootstrapChoice `json:"models"`
	Efforts []appBootstrapChoice `json:"efforts"`
	Access  []appBootstrapChoice `json:"access"`
}

type appBootstrap struct {
	Organization  appBootstrapOrganization   `json:"organization"`
	Organizations []appBootstrapOrganization `json:"organizations"`
	Actor         appBootstrapActor          `json:"actor"`
	Projects      []appBootstrapProject      `json:"projects"`
	Support       *appBootstrapSupport       `json:"support"`
	CSRFToken     string                     `json:"csrf_token,omitempty"`
	Capabilities  appBootstrapCapabilities   `json:"capabilities"`
	Preferences   appBootstrapPreferences    `json:"preferences"`
	Feature       appBootstrapFeature        `json:"feature"`
	Plan          *appBootstrapPlan          `json:"plan"`
	APIBase       string                     `json:"api_base"`
	BasePath      string                     `json:"base_path"`
	SignInPath    string                     `json:"sign_in_path"`
	Version       string                     `json:"version,omitempty"`
}

// appBootstrapPayload implements GET /app/bootstrap and its /chat/bootstrap
// alias for the hosted session.
// appBootstrapRefusal is the 403 a signed-in account without organization
// access receives. It names the organization and carries the session's CSRF
// token so the support screen, the one screen such an account can use, can
// start a support session.
type appBootstrapRefusal struct {
	Code    string                     `json:"code"`
	Message string                     `json:"message"`
	Details appBootstrapRefusalDetails `json:"details"`
}

type appBootstrapRefusalDetails struct {
	Organization string `json:"organization"`
	CSRFToken    string `json:"csrf_token,omitempty"`
}

func (s *Service) appBootstrapPayload(c echo.Context) error {
	credential, status, err := s.hostedCredential(c.Request().Context(), c)
	if err != nil {
		if status == http.StatusForbidden {
			return c.JSON(http.StatusForbidden, appBootstrapRefusal{Code: "forbidden", Message: "This account has no access to this organization", Details: appBootstrapRefusalDetails{Organization: s.config.Hosted.OrganizationID, CSRFToken: s.hostedPageCSRF(c)}})
		}
		return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "unauthorized", Message: "A hosted session is required"})
	}
	session, ok := c.Get("hosted_session").(auth.Session)
	if !ok {
		return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "unauthorized", Message: "A hosted session is required"})
	}
	csrf := s.hostedPageCSRF(c)
	if csrf == "" {
		return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "unauthorized", Message: "A hosted session is required"})
	}
	payload, err := s.readAppBootstrap(c.Request().Context(), credential, session)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	payload.CSRFToken = csrf
	payload.Actor.PlatformRole = sharedPlatformRole(c)
	if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "action", "GET "+c.Path(), "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, payload)
}

func (s *Service) readAppBootstrap(ctx context.Context, credential apiCredential, session auth.Session) (appBootstrap, error) {
	organization := s.config.Hosted.OrganizationID
	manage := credential.HostedRole == "owner" || credential.HostedRole == "admin"
	payload := appBootstrap{
		Organization:  appBootstrapOrganization{ID: organization, Name: organization, PublicURL: s.config.Hosted.PublicURL, Current: true},
		Organizations: []appBootstrapOrganization{},
		Actor: appBootstrapActor{
			PrincipalID: operatorIdentity(credential, organization).PrincipalID, Subject: credential.Hosted.Subject, Email: session.Email, Role: credential.HostedRole,
			CanManage:        manage,
			CanManageRunners: credential.HostedRole != "viewer" && s.appAllRunnerGrants(ctx, credential),
		},
		Projects:   []appBootstrapProject{},
		APIBase:    "/api/v2/organizations/" + organization,
		BasePath:   s.hostedBase(),
		SignInPath: s.hostedSignInPath(),
		Version:    s.config.Version,
	}
	if err := s.database.db.QueryRowContext(ctx, "SELECT name FROM organizations WHERE id = ?", organization).Scan(&payload.Organization.Name); err != nil {
		return appBootstrap{}, fmt.Errorf("read organization name: %w", err)
	}
	if identity := credential.Hosted; identity.SupportActor != "" {
		payload.Support = &appBootstrapSupport{Actor: identity.SupportActor, Reason: identity.SupportReason, ExpiresAt: identity.ExpiresAt.UTC().Format(time.RFC3339)}
	}
	var err error
	payload.Projects, err = s.hostedReadableProjects(ctx, credential)
	if err != nil {
		return appBootstrap{}, err
	}
	if credential.HostedKeyScope == "" {
		payload.Organizations, err = s.hostedOrganizationChoices(ctx, session)
	}
	if err != nil {
		return appBootstrap{}, err
	}
	for index := range payload.Organizations {
		if payload.Organizations[index].Current {
			payload.Organizations[index].Name = payload.Organization.Name
		}
	}
	payload.Capabilities.Coordinator = s.conversations != nil && s.conversations.coordinator != nil && s.conversations.coordinator.Available()
	payload.Feature = s.appBootstrapFeature()
	payload.Preferences, err = s.appBootstrapPreferences(ctx, organization, payload.Projects)
	if err != nil {
		return appBootstrap{}, err
	}
	payload.Plan = s.appBootstrapPlan(ctx)
	return payload, nil
}

// appBootstrapPreferences publishes the turn preference choices: every model
// the organization's enrolled runners report for the projects the actor can
// read, and the fixed effort and access vocabularies. A hub whose runners
// report no models still answers with "auto" and, when it knows one, the
// configured default (decisions section 14).
func (s *Service) appBootstrapPreferences(ctx context.Context, organization string, readable []appBootstrapProject) (appBootstrapPreferences, error) {
	preferences := appBootstrapPreferences{
		Models:  []appBootstrapChoice{},
		Efforts: []appBootstrapChoice{},
		Access:  []appBootstrapChoice{},
	}
	if s.conversations == nil {
		return preferences, nil
	}
	projects := make([]string, 0, len(readable))
	for _, project := range readable {
		projects = append(projects, project.ID)
	}
	now, err := s.database.currentTime()
	if err != nil {
		return preferences, err
	}
	models, err := s.conversationModelChoices(ctx, s.database.db, tracker.OrganizationID(organization), projects, now)
	if err != nil {
		return preferences, err
	}
	preferences.Models = appBootstrapModelChoices(models, s.conversationDefaultModel())
	preferences.Efforts = appBootstrapChoices(conversation.ReasoningEfforts()[1:], conversationChoiceLabel, strings.TrimSpace(s.conversations.config.ReasoningEffort))
	preferences.Access = appBootstrapChoices(conversation.AccessLevels()[1:], conversationChoiceLabel, "")
	return preferences, nil
}

// appBootstrapModelChoices renders the model picker: "auto" first, then every
// reported model with whatever its runner's catalog said about it.
func appBootstrapModelChoices(models []conversationModel, configured string) []appBootstrapChoice {
	known := configured != "" && slices.ContainsFunc(models, func(model conversationModel) bool { return model.ID == configured })
	choices := make([]appBootstrapChoice, 0, len(models)+1)
	choices = append(choices, appBootstrapChoice{ID: conversation.PreferenceAuto, Label: "Auto", Default: !known})
	for _, model := range models {
		choices = append(choices, appBootstrapChoice{
			ID:             model.ID,
			Label:          conversationModelLabel(model),
			Default:        known && model.ID == configured,
			Efforts:        model.Efforts,
			DefaultEffort:  model.DefaultEffort,
			Provider:       model.Provider,
			Legacy:         model.Legacy,
			BackendDefault: model.BackendDefault,
		})
	}
	return choices
}

// appBootstrapChoices renders one picker: "auto" first, then the values, with
// the default flag on the configured value or on "auto" when there is none.
func appBootstrapChoices(values []string, label func(string) string, configured string) []appBootstrapChoice {
	known := configured != "" && slices.Contains(values, configured)
	choices := make([]appBootstrapChoice, 0, len(values)+1)
	choices = append(choices, appBootstrapChoice{ID: conversation.PreferenceAuto, Label: "Auto", Default: !known})
	for _, value := range values {
		choices = append(choices, appBootstrapChoice{ID: value, Label: label(value), Default: known && value == configured})
	}
	return choices
}

// conversationModelLabel is what a model picker shows: the label the backend
// catalog gave, and otherwise the identifier, which is the vocabulary
// operators already use.
func conversationModelLabel(model conversationModel) string {
	if label := strings.TrimSpace(model.Label); label != "" {
		return label
	}
	return model.ID
}

// conversationChoiceLabel capitalizes a fixed vocabulary value: "read_only"
// becomes "Read only".
func conversationChoiceLabel(value string) string {
	words := strings.Split(strings.ReplaceAll(value, "_", " "), " ")
	if len(words) > 0 && words[0] != "" {
		words[0] = strings.ToUpper(words[0][:1]) + words[0][1:]
	}
	return strings.Join(words, " ")
}

// appBootstrapPlan summarizes the effective entitlement. A hub without hosted
// plans reports no plan rather than failing the bootstrap.
func (s *Service) appBootstrapPlan(ctx context.Context) *appBootstrapPlan {
	if s.database.hostedPlans == nil {
		return nil
	}
	entitlement, err := s.database.hostedPlanUsage(ctx, s.config.now())
	if err != nil {
		s.config.Logger.Warn("hosted plan summary is unavailable", "error", err)
		return nil
	}
	return &appBootstrapPlan{
		ID:           entitlement.EffectiveBase.ID,
		Name:         entitlement.Name,
		Source:       entitlement.Source,
		WindowEndsAt: entitlement.WindowEndsAt.UTC().Format(time.RFC3339),
	}
}

// hostedReadableProjects lists the projects the credential may read with the
// capabilities the client needs to render them.
func (s *Service) hostedReadableProjects(ctx context.Context, credential apiCredential) ([]appBootstrapProject, error) {
	projects := []appBootstrapProject{}
	rows, err := s.database.db.QueryContext(ctx, `SELECT p.id, p.name, p.profile, p.states_json, g.can_write, g.manage_runner
FROM projects p JOIN hosted_project_grants g ON g.project_id = p.id
WHERE g.user_id = ? AND p.organization_id = ? ORDER BY p.name, p.id`, credential.Hosted.Subject, s.config.Hosted.OrganizationID)
	if err != nil {
		return nil, fmt.Errorf("list project grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var project appBootstrapProject
		var states string
		if err := rows.Scan(&project.ID, &project.Name, &project.Profile, &states, &project.CanWrite, &project.CanManageRunners); err != nil {
			return nil, fmt.Errorf("scan project grant: %w", err)
		}
		if err := json.Unmarshal([]byte(states), &project.States); err != nil {
			return nil, fmt.Errorf("decode project states: %w", err)
		}
		// Viewers never write or manage runners regardless of the grant flag
		// (requireHostedProject, requireHostedAdministration).
		viewer := credential.HostedRole == "viewer"
		project.CanWrite = project.CanWrite && !viewer
		project.CanManageRunners = project.CanManageRunners && !viewer
		projects = append(projects, project)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list project grants: %w", err)
	}
	if credential.HostedKeyScope != "" {
		filtered := projects[:0]
		for _, project := range projects {
			scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), project: tracker.ProjectID(project.ID), credential: credential}
			if err := s.database.authorizeNativeProject(ctx, scope); err != nil {
				var native *nativeError
				if errors.As(err, &native) && native.Code == "not_found" {
					continue
				}
				return nil, err
			}
			filtered = append(filtered, project)
		}
		projects = filtered
	}
	for index := range projects {
		project := &projects[index]
		project.Capabilities, err = s.hostedWorkspaceCapabilities(ctx, credential, *project)
		if err != nil {
			return nil, err
		}
	}
	return projects, nil
}

func (s *Service) hostedWorkspaceCapabilities(ctx context.Context, credential apiCredential, project appBootstrapProject) (workspacesession.Capabilities, error) {
	result := workspacesession.Capabilities{}
	if s.workspaces == nil || !project.CanWrite || credential.Hosted == nil || credential.HostedRole == "viewer" {
		return result, nil
	}
	rows, err := s.database.db.QueryContext(ctx, `SELECT r.id FROM runner_identities r
JOIN token_grants g ON g.token_id = r.token_id AND g.organization_id = r.organization_id
WHERE r.organization_id = ? AND r.removed_at IS NULL AND g.project_id = ?`, s.config.Hosted.OrganizationID, project.ID)
	if err != nil {
		return result, fmt.Errorf("list workspace runners: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return result, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return result, err
	}
	now := s.config.now()
	for _, id := range ids {
		runner, err := readRunnerWithClock(ctx, s.database.db, tracker.OrganizationID(s.config.Hosted.OrganizationID), id, s.config.now)
		if err != nil {
			return result, err
		}
		if runner.Health != "online" || runner.State != "active" || !slices.Contains(runner.Operations, "claim") {
			continue
		}
		capabilities, isolation, fresh, err := runnerWorkspaceCapabilities(ctx, s.database.db, id, now)
		if err != nil {
			return result, err
		}
		if !fresh {
			continue
		}
		files := capabilities.Satisfies([]string{workspacesession.CapabilityFiles, workspacesession.CapabilityExec})
		result.Files = result.Files || files
		result.Exec = result.Exec || files
		terminal := files && capabilities.Terminal && project.CanManageRunners && s.workspaces.config.Terminal.Enabled &&
			terminalIsolationAllowed([]string{workspacesession.CapabilityTerminal}, s.workspaces.config.Terminal.Isolation, isolation)
		if s.workspaces.config.Terminal.Isolation == workspacesession.IsolationUser && credential.HostedRole != "owner" && credential.HostedRole != "admin" {
			terminal = false
		}
		result.Terminal = result.Terminal || terminal
	}
	return result, nil
}

// hostedOrganizationChoices lists the directory destinations the signed-in
// subject belongs to. A support session never switches organizations.
func (s *Service) hostedOrganizationChoices(ctx context.Context, session auth.Session) ([]appBootstrapOrganization, error) {
	choices := []appBootstrapOrganization{}
	if session.Identity == nil || session.Identity.SupportActor != "" || s.hostedShared() {
		return choices, nil
	}
	memberships, err := s.config.Hosted.Provider.Memberships(ctx, session.Identity.Subject, "")
	if err != nil {
		return nil, fmt.Errorf("read organization memberships: %w", err)
	}
	for _, destination := range s.config.Hosted.Directory {
		for _, membership := range memberships {
			if membership.OrganizationID != destination.WorkOSOrganizationID || membership.UserID != session.Identity.Subject || membership.Status != "active" {
				continue
			}
			choices = append(choices, appBootstrapOrganization{
				ID: destination.OrganizationID, Name: destination.OrganizationID, PublicURL: destination.PublicURL,
				Current: destination.OrganizationID == s.config.Hosted.OrganizationID,
			})
			break
		}
	}
	return choices, nil
}

func sharedPlatformRole(c echo.Context) string {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindBrowser || claims.SupportActor != "" {
		return ""
	}
	return claims.PlatformRole
}
