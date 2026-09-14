package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
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
	appReservedPrefixes = []string{"/api/", "/auth/", "/webhooks/", "/static/"}
	appReservedPaths    = []string{"/health", "/invite", "/logout"}
)

// appReserved reports whether a path belongs to the hub rather than the
// client. A reserved prefix owns its whole subtree; a reserved path owns only
// itself, so a client route that merely starts with the same letters stays a
// client route.
func appReserved(path string) bool {
	return slices.Contains(appReservedPaths, path) || slices.ContainsFunc(appReservedPrefixes, func(prefix string) bool {
		return strings.HasPrefix(path, prefix)
	})
}

// registerAppRoutes mounts the React application: its bootstrap payload and
// the catch-all shell. The hosted hub serves one frontend for every screen
// (decisions section 11), so the shell is mounted whether or not the
// conversation product is enabled. A single-tenant hub has no hosted session
// and mounts none of this.
func (s *Service) registerAppRoutes(e *echo.Echo) {
	e.GET("/app/bootstrap", s.appBootstrapPayload)
	e.GET("/chat/bootstrap", s.appBootstrapPayload)
	e.GET("/app/updates", s.appUpdates)
	// Any method, so an unmatched path still answers not found rather than
	// the method-not-allowed a GET-only wildcard would produce.
	e.Any("/", s.appShell)
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
		if _, _, err := s.hostedSession(c); err != nil {
			return c.Redirect(http.StatusSeeOther, "/login")
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
	return c.HTMLBlob(http.StatusOK, content)
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
	RunnerID    string `json:"runner_id"`
	DisplayName string `json:"display_name"`
	// Version is what the host's heartbeat last reported, empty for a runner
	// that has never reported one.
	Version string `json:"version"`
	Online  bool   `json:"online"`
	Behind  bool   `json:"behind"`
}

type appUpdates struct {
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

// runnerBehind reports whether a runner's build is behind the hub's. It is the
// same comparison `detent doctor` makes between a running and an installed
// binary (buildinfo.DetectDrift), so a placeholder version on either side is
// "cannot tell" rather than "behind": an unversioned development build must
// never light up an update badge on somebody's fleet.
func runnerBehind(current string, reported string) bool {
	drift := buildinfo.DetectDrift(buildinfo.Info{Version: reported}, buildinfo.Info{Version: current})
	return drift.Comparable && drift.Detected
}

// appUpdates implements GET /app/updates for the footer's pill: which enrolled
// runners are not on the hub's build.
//
// It is gated on the session alone and reads one row per runner. The fleet
// screen's own endpoint answers the same fact in much more detail — leases,
// provider capacity, per-runner grants — and re-reading all of that every few
// minutes for a badge would be the wrong trade. A revoked enrolment is not in
// the answer: it is not a runner anybody is going to upgrade.
func (s *Service) appUpdates(c echo.Context) error {
	if _, _, err := s.hostedSession(c); err != nil {
		return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "unauthorized", Message: "A hosted session is required"})
	}
	current := detentVersion(s.config.Version)
	payload := appUpdates{Current: current, Source: "hub", Runners: []appUpdateRunner{}, Client: s.clientBuild}
	rows, err := s.database.db.QueryContext(c.Request().Context(), `SELECT r.id, r.display_name, m.version, r.last_heartbeat_at
FROM runner_identities r JOIN machines m ON m.id = r.machine_id JOIN api_tokens t ON t.id = r.token_id
WHERE r.organization_id = ? AND t.revoked_at IS NULL ORDER BY r.display_name, r.id`, s.config.Hosted.OrganizationID)
	if err != nil {
		return s.nativeAPIError(c, fmt.Errorf("list runner versions: %w", err))
	}
	defer func() { _ = rows.Close() }()
	now := s.config.now()
	for rows.Next() {
		var runner appUpdateRunner
		var heartbeat string
		if err := rows.Scan(&runner.RunnerID, &runner.DisplayName, &runner.Version, &heartbeat); err != nil {
			return s.nativeAPIError(c, fmt.Errorf("scan runner version: %w", err))
		}
		at, err := parseTimeValue(heartbeat)
		if err != nil {
			return s.nativeAPIError(c, fmt.Errorf("read runner heartbeat: %w", err))
		}
		// The same window readRunner calls "offline".
		runner.Online = !now.Before(at) && now.Before(at.Add(runnerauth.HeartbeatTimeout))
		runner.Behind = runnerBehind(current, runner.Version)
		if runner.Behind {
			payload.BehindCount++
		}
		payload.Runners = append(payload.Runners, runner)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return s.nativeAPIError(c, fmt.Errorf("list runner versions: %w", err))
	}
	return c.JSON(http.StatusOK, payload)
}

// Bootstrap payload (decisions section 12).

type appBootstrapOrganization struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PublicURL string `json:"public_url"`
	Current   bool   `json:"current"`
}

type appBootstrapActor struct {
	PrincipalID      string `json:"principal_id"`
	Subject          string `json:"subject"`
	Email            string `json:"email"`
	Role             string `json:"role"`
	CanManage        bool   `json:"can_manage"`
	CanManageRunners bool   `json:"can_manage_runners"`
}

type appBootstrapProject struct {
	ID               string                `json:"id"`
	Name             string                `json:"name"`
	Profile          string                `json:"profile"`
	CanWrite         bool                  `json:"can_write"`
	CanManageRunners bool                  `json:"can_manage_runners"`
	States           []tracker.NativeState `json:"states"`
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
	CSRFToken     string                     `json:"csrf_token"`
	Capabilities  appBootstrapCapabilities   `json:"capabilities"`
	Preferences   appBootstrapPreferences    `json:"preferences"`
	Feature       appBootstrapFeature        `json:"feature"`
	Plan          *appBootstrapPlan          `json:"plan"`
	APIBase       string                     `json:"api_base"`
	Version       string                     `json:"version,omitempty"`
}

// appBootstrapPayload implements GET /app/bootstrap and its /chat/bootstrap
// alias for the hosted session.
func (s *Service) appBootstrapPayload(c echo.Context) error {
	credential, status, err := s.hostedCredential(c)
	if err != nil {
		if status == http.StatusForbidden {
			return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "forbidden", Message: "This account has no access to this organization"})
		}
		return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "unauthorized", Message: "A hosted session is required"})
	}
	session, ok := c.Get("hosted_session").(auth.Session)
	if !ok {
		return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "unauthorized", Message: "A hosted session is required"})
	}
	cookie, err := c.Cookie(hostedCookie)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "unauthorized", Message: "A hosted session is required"})
	}
	ctx := c.Request().Context()
	organization := s.config.Hosted.OrganizationID
	manage := credential.HostedRole == "owner" || credential.HostedRole == "admin"
	payload := appBootstrap{
		Organization:  appBootstrapOrganization{ID: organization, Name: organization, PublicURL: s.config.Hosted.PublicURL, Current: true},
		Organizations: []appBootstrapOrganization{},
		Actor: appBootstrapActor{
			PrincipalID: credential.ID, Subject: credential.Hosted.Subject, Email: session.Email, Role: credential.HostedRole,
			CanManage:        manage,
			CanManageRunners: credential.HostedRole != "viewer" && s.hostedRunnerGrants(ctx, credential, nil),
		},
		Projects:  []appBootstrapProject{},
		CSRFToken: hostedCSRF(cookie.Value),
		Feature:   appBootstrapFeature{Conversation: s.conversations != nil},
		APIBase:   "/api/v2/organizations/" + organization,
		Version:   s.config.Version,
	}
	if err := s.database.db.QueryRowContext(ctx, "SELECT name FROM organizations WHERE id = ?", organization).Scan(&payload.Organization.Name); err != nil {
		return s.nativeAPIError(c, fmt.Errorf("read organization name: %w", err))
	}
	if identity := credential.Hosted; identity.SupportActor != "" {
		payload.Support = &appBootstrapSupport{Actor: identity.SupportActor, Reason: identity.SupportReason, ExpiresAt: identity.ExpiresAt.UTC().Format(time.RFC3339)}
	}
	payload.Projects, err = s.hostedReadableProjects(ctx, credential)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	payload.Organizations, err = s.hostedOrganizationChoices(ctx, session)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	for index := range payload.Organizations {
		if payload.Organizations[index].Current {
			payload.Organizations[index].Name = payload.Organization.Name
		}
	}
	payload.Capabilities, err = s.appBootstrapCapabilities(ctx, organization, payload.Projects)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	payload.Preferences, err = s.appBootstrapPreferences(ctx, organization, payload.Projects)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	payload.Plan = s.appBootstrapPlan(ctx)
	if err := s.hostedAudit(ctx, credential.Hosted, "action", "GET "+c.Path(), "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, payload)
}

// appBootstrapCapabilities reports whether a coordinator turn can run: on the
// hub's own backend, or on an enrolled runner able to take one (decisions
// section 9.2).
func (s *Service) appBootstrapCapabilities(ctx context.Context, organization string, readable []appBootstrapProject) (appBootstrapCapabilities, error) {
	capabilities := appBootstrapCapabilities{}
	if s.conversations == nil {
		return capabilities, nil
	}
	// Attachments are part of the conversation product, not of a backend: a
	// hub that serves conversations serves the dropzone (section 17.1).
	capabilities.Attachments = true
	if capabilities.Coordinator = s.conversations.coordinator.Available(); capabilities.Coordinator {
		return capabilities, nil
	}
	projects := make([]string, 0, len(readable))
	for _, project := range readable {
		projects = append(projects, project.ID)
	}
	now, err := s.database.currentTime()
	if err != nil {
		return capabilities, err
	}
	capabilities.Coordinator, err = liveControlRunnerEnrolled(ctx, s.database.db, tracker.OrganizationID(organization), projects, now)
	return capabilities, err
}

// appBootstrapPreferences publishes the turn preference choices: every model
// the organization's enrolled runners report for the projects the actor can
// read, and the fixed effort and access vocabularies. A hub whose runners
// report no models still answers with "auto" and, when it knows one, the
// project-configured default (decisions section 14).
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

// conversationChoiceLabel title-cases a fixed vocabulary value: "read_only"
// becomes "Read only".
func conversationChoiceLabel(value string) string {
	words := strings.Split(strings.ReplaceAll(value, "_", " "), " ")
	for index, word := range words {
		if index == 0 && word != "" {
			words[index] = strings.ToUpper(word[:1]) + word[1:]
		}
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
		Name:         fmt.Sprintf("%s · version %d", entitlement.EffectiveBase.ID, entitlement.EffectiveBase.Version),
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
	return projects, nil
}

// hostedOrganizationChoices lists the directory destinations the signed-in
// subject belongs to. A support session never switches organizations.
func (s *Service) hostedOrganizationChoices(ctx context.Context, session auth.Session) ([]appBootstrapOrganization, error) {
	choices := []appBootstrapOrganization{}
	if session.Identity == nil || session.Identity.SupportActor != "" {
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
