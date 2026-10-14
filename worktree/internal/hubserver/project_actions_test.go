package hubserver

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Project actions end to end (decisions section 18.12): the catalogue a
// project authors, the bounds that keep it a menu rather than a table nobody
// can read, and the write gate that decides who may add a command the whole
// project runs.

// actionFixture is the workspace fixture with the action endpoints wrapped, so
// the catalogue and the runs that need a workspace share one hub.
type actionFixture struct {
	*workspaceRunnerFixture
}

func newActionFixture(t *testing.T) actionFixture {
	t.Helper()
	return actionFixture{newWorkspaceRunnerFixture(t)}
}

// execCapabilities is what a runner that serves project actions reports. The
// default fixture deliberately does not: exec is a surface a runner has to
// offer, exactly as the terminal is.
var execCapabilities = workspacesession.Capabilities{Files: true, Diff: true, Exec: true}

// postAction sends one POST /actions with its own idempotency key, because the
// key is scoped to the actor and the route: a shared one would replay the
// first action rather than author a second.
func (f actionFixture) postAction(t *testing.T, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	filled := map[string]any{"idempotency_key": newNativeID("actkey")}
	maps.Copy(filled, body)
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/actions", token, filled)
}

func (f actionFixture) authored(t *testing.T, body map[string]any) workspacesession.Action {
	t.Helper()
	response := f.postAction(t, f.token, body)
	requireNativeStatus(t, response, http.StatusCreated)
	var action workspacesession.Action
	decodeHubResponse(t, response, &action)
	return action
}

func (f actionFixture) patchAction(t *testing.T, id string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	filled := map[string]any{"idempotency_key": newNativeID("actkey")}
	maps.Copy(filled, body)
	return performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/actions/"+id, f.token, filled)
}

func (f actionFixture) actions(t *testing.T) projectActionList {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/actions", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var listing projectActionList
	decodeHubResponse(t, response, &listing)
	return listing
}

// A created action carries every normalized field, starts at revision 1 and
// records who authored it.
func TestProjectActionCreateNormalizesAndRecordsProvenance(t *testing.T) {
	t.Parallel()
	f := newActionFixture(t)

	action := f.authored(t, map[string]any{
		"name": "  Run tests  ", "command": "  go test ./...  ", "keybinding": " MOD+Shift+T ",
		"icon": " TEST ", "preview_url": " https://preview.example.test/app ",
		"open_preview": true, "run_on_worktree_creation": true,
	})
	if action.Name != "Run tests" || action.Command != "go test ./..." {
		t.Fatalf("action = %#v, want the trimmed name and command", action)
	}
	// The chord is lower-cased so a keystroke resolves the same way whatever
	// case the author typed.
	if action.Keybinding != "mod+shift+t" || action.Icon != "test" {
		t.Fatalf("action = %#v, want the folded chord and icon", action)
	}
	if action.PreviewURL != "https://preview.example.test/app" || !action.OpenPreview || !action.RunOnWorktreeCreation {
		t.Fatalf("action = %#v, want the preview fields carried", action)
	}
	if action.Revision != 1 || action.CreatedBy != f.ownerID || action.CreatedAt.IsZero() {
		t.Fatalf("action provenance = %#v", action)
	}
	if !strings.HasPrefix(action.ID, "action_") {
		t.Fatalf("action id = %q, want a typed identifier", action.ID)
	}

	t.Run("an omitted icon defaults to the glyph the dialog opens with", func(t *testing.T) {
		plain := f.authored(t, map[string]any{"name": "Build", "command": "make build"})
		if plain.Icon != "play" || plain.Keybinding != "" || plain.PreviewURL != "" {
			t.Fatalf("action = %#v, want the defaults", plain)
		}
	})

	t.Run("the listing is authoring order", func(t *testing.T) {
		// A project that wants install before build writes install first, and
		// the listing is the only thing that says so.
		local := newActionFixture(t)
		first := local.authored(t, map[string]any{"name": "Install", "command": "npm install"})
		second := local.authored(t, map[string]any{"name": "Build", "command": "npm run build"})
		listing := local.actions(t)
		if len(listing.Items) != 2 || listing.Items[0].ID != first.ID || listing.Items[1].ID != second.ID {
			t.Fatalf("listing = %#v, want %s then %s", listing.Items, first.ID, second.ID)
		}
	})
}

// Every field the contract normalizes is refused through the API, not only in
// the normalizer: a field nobody routed to its normalizer would pass.
func TestProjectActionCreateRefusals(t *testing.T) {
	t.Parallel()
	f := newActionFixture(t)

	for _, test := range []struct {
		name string
		body map[string]any
	}{
		{"an empty name", map[string]any{"name": "   ", "command": "go test ./..."}},
		{"an empty command", map[string]any{"name": "Tests", "command": "  "}},
		{"a command past the byte bound", map[string]any{"name": "Tests",
			"command": strings.Repeat("a", workspacesession.MaxExecCommandBytes+1)}},
		{"a name past the byte bound", map[string]any{"command": "go test ./...",
			"name": strings.Repeat("n", workspacesession.MaxActionNameBytes+1)}},
		{"an unknown icon", map[string]any{"name": "Tests", "command": "go test ./...", "icon": "rocket"}},
		{"a preview URL that is not http", map[string]any{"name": "Tests", "command": "go test ./...",
			"preview_url": "ftp://preview.example.test"}},
		{"a preview URL past the byte bound", map[string]any{"name": "Tests", "command": "go test ./...",
			"preview_url": "https://example.test/" + strings.Repeat("p", workspacesession.MaxActionPreviewURLBytes)}},
		{"a keybinding with whitespace in it", map[string]any{"name": "Tests", "command": "go test ./...",
			"keybinding": "mod+shift t"}},
		{"a keybinding past the byte bound", map[string]any{"name": "Tests", "command": "go test ./...",
			"keybinding": strings.Repeat("k", workspacesession.MaxActionKeybindingBytes+1)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeCode(t, f.postAction(t, f.token, test.body), http.StatusUnprocessableEntity, "invalid_request")
		})
	}

	t.Run("a chord another action already claims is refused and the refusal names it", func(t *testing.T) {
		local := newActionFixture(t)
		local.authored(t, map[string]any{"name": "Run tests", "command": "go test ./...", "keybinding": "mod+t"})
		response := local.postAction(t, local.token, map[string]any{
			"name": "Run e2e", "command": "npm run e2e", "keybinding": "MOD+T"})
		requireNativeCode(t, response, http.StatusUnprocessableEntity, "invalid_request")
		var failure nativeError
		decodeHubResponse(t, response, &failure)
		// The author's next move is to change one of the two, so the answer
		// has to say which action is holding the chord.
		if !strings.Contains(failure.Message, "Run tests") || !strings.Contains(failure.Message, "mod+t") {
			t.Fatalf("message = %q, want the conflicting action and chord named", failure.Message)
		}
	})

	t.Run("two actions may both have no chord", func(t *testing.T) {
		// The unique index is partial for this reason: no shortcut is not a
		// shortcut two actions are fighting over.
		local := newActionFixture(t)
		local.authored(t, map[string]any{"name": "One", "command": "make one"})
		local.authored(t, map[string]any{"name": "Two", "command": "make two"})
		if listing := local.actions(t); len(listing.Items) != 2 {
			t.Fatalf("listing = %#v, want both actions", listing.Items)
		}
	})
}

// The catalogue is bounded: the header menu lists every action and a
// keybinding is registered per action, so an unbounded set is an unbounded
// menu.
func TestProjectActionCreateRefusesPastTheProjectCap(t *testing.T) {
	t.Parallel()
	f := newActionFixture(t)
	for range workspacesession.MaxActionsPerProject {
		f.authored(t, map[string]any{"name": "Action", "command": "make target"})
	}
	requireNativeCode(t, f.postAction(t, f.token, map[string]any{"name": "One more", "command": "make more"}),
		http.StatusUnprocessableEntity, "invalid_request")
	if listing := f.actions(t); len(listing.Items) != workspacesession.MaxActionsPerProject {
		t.Fatalf("listing held %d actions, want %d", len(listing.Items), workspacesession.MaxActionsPerProject)
	}
}

// A PATCH changes what it names and nothing else, bumps the revision, and is
// fenced by the revision the author read.
func TestProjectActionPatch(t *testing.T) {
	t.Parallel()
	f := newActionFixture(t)

	t.Run("an absent field keeps what is stored and a present empty one clears it", func(t *testing.T) {
		action := f.authored(t, map[string]any{"name": "Run tests", "command": "go test ./...",
			"keybinding": "mod+1", "icon": "test", "run_on_worktree_creation": true})
		response := f.patchAction(t, action.ID, map[string]any{"expected_revision": 1, "name": "Run unit tests"})
		requireNativeStatus(t, response, http.StatusOK)
		var patched workspacesession.Action
		decodeHubResponse(t, response, &patched)
		if patched.Name != "Run unit tests" || patched.Revision != 2 {
			t.Fatalf("patched = %#v, want the new name at revision 2", patched)
		}
		// A dialog that only changed the name must not drop the chord the
		// author set last week.
		if patched.Keybinding != "mod+1" || patched.Icon != "test" || !patched.RunOnWorktreeCreation {
			t.Fatalf("patched = %#v, want every unnamed field kept", patched)
		}
		response = f.patchAction(t, action.ID, map[string]any{"expected_revision": 2,
			"keybinding": "", "run_on_worktree_creation": false})
		requireNativeStatus(t, response, http.StatusOK)
		// A fresh value, not the one above: a cleared chord is omitted from
		// the resource, and decoding over a struct that still held the old one
		// would read the old one back.
		var emptied workspacesession.Action
		decodeHubResponse(t, response, &emptied)
		if emptied.Keybinding != "" || emptied.RunOnWorktreeCreation || emptied.Revision != 3 {
			t.Fatalf("patched = %#v, want the chord and the flag cleared at revision 3", emptied)
		}
		if emptied.Name != "Run unit tests" || emptied.Icon != "test" {
			t.Fatalf("patched = %#v, want the unnamed fields still kept", emptied)
		}
	})

	t.Run("an action keeping its own chord is not a conflict with itself", func(t *testing.T) {
		action := f.authored(t, map[string]any{"name": "Lint", "command": "make lint", "keybinding": "mod+l"})
		requireNativeStatus(t, f.patchAction(t, action.ID, map[string]any{
			"expected_revision": 1, "command": "make lint-fix", "keybinding": "mod+l"}), http.StatusOK)
	})

	t.Run("a chord another action holds is refused", func(t *testing.T) {
		f.authored(t, map[string]any{"name": "Debug", "command": "make debug", "keybinding": "mod+d"})
		action := f.authored(t, map[string]any{"name": "Configure", "command": "make configure"})
		requireNativeCode(t, f.patchAction(t, action.ID, map[string]any{
			"expected_revision": 1, "keybinding": "mod+d"}), http.StatusUnprocessableEntity, "invalid_request")
	})

	t.Run("a stale expected_revision is a conflict that names the current one", func(t *testing.T) {
		action := f.authored(t, map[string]any{"name": "Build", "command": "make build"})
		requireNativeStatus(t, f.patchAction(t, action.ID, map[string]any{"expected_revision": 1, "name": "Build all"}), http.StatusOK)
		response := f.patchAction(t, action.ID, map[string]any{"expected_revision": 1, "name": "Build some"})
		requireNativeCode(t, response, http.StatusConflict, "revision_conflict")
		var failure nativeError
		decodeHubResponse(t, response, &failure)
		if failure.CurrentRevision != 2 {
			t.Fatalf("current_revision = %d, want 2", failure.CurrentRevision)
		}
	})

	t.Run("a non-positive expected_revision is refused rather than treated as absent", func(t *testing.T) {
		action := f.authored(t, map[string]any{"name": "Test", "command": "make test"})
		requireNativeCode(t, f.patchAction(t, action.ID, map[string]any{"expected_revision": 0, "name": "Test all"}),
			http.StatusUnprocessableEntity, "invalid_request")
	})

	t.Run("a normalizer refusal reaches a patch too", func(t *testing.T) {
		action := f.authored(t, map[string]any{"name": "Test", "command": "make test"})
		requireNativeCode(t, f.patchAction(t, action.ID, map[string]any{"expected_revision": 1, "command": "   "}),
			http.StatusUnprocessableEntity, "invalid_request")
	})

	t.Run("an action of another project is not found", func(t *testing.T) {
		requireNativeCode(t, f.patchAction(t, newNativeID("action"), map[string]any{
			"expected_revision": 1, "name": "Nothing"}), http.StatusNotFound, "not_found")
	})
}

// Deleting an action removes it and its runs, which the migration declares
// with ON DELETE CASCADE: a run of an action nobody can name is a row no
// surface could ever show.
func TestProjectActionDeleteCascadesItsRuns(t *testing.T) {
	t.Parallel()
	f := newActionFixture(t)
	action := f.authored(t, map[string]any{"name": "Run tests", "command": "go test ./..."})
	workspace := f.execWorkspace(t)
	run := f.queueRun(t, action.ID, workspace)

	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete,
		f.base+"/actions/"+action.ID, f.token, nil), http.StatusNoContent)
	if listing := f.actions(t); len(listing.Items) != 0 {
		t.Fatalf("listing = %#v, want the action gone", listing.Items)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet,
		f.base+"/actions/"+action.ID+"/runs/"+run, f.token, nil), http.StatusNotFound)
	var rows int
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM project_action_runs WHERE id = ?", run).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("the action's runs survived it: %d rows", rows)
	}

	t.Run("deleting an action that is not there is not found", func(t *testing.T) {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete,
			f.base+"/actions/"+action.ID, f.token, nil), http.StatusNotFound)
	})
}

// Authoring an action is a project write: it adds a command the whole project
// runs, so a reader and a viewer are refused exactly as they are for an issue.
func TestProjectActionWriteGateRefusesAViewerAndAReadOnlyGrant(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		role  string
		grant string
		read  int
		write int
	}{
		{name: "a member with write may author", role: "member", grant: "write", read: http.StatusOK, write: http.StatusCreated},
		{name: "a viewer may read and never write", role: "viewer", grant: "write", read: http.StatusOK, write: http.StatusNotFound},
		{name: "a read-only grant may read and never write", role: "member", grant: "read", read: http.StatusOK, write: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			user := f.user(t, test.role, test.role, test.role+"@example.test", test.grant, "")
			requireNativeStatus(t, f.request(t, user, http.MethodGet, f.base+"/actions", nil), test.read)
			requireNativeStatus(t, f.request(t, user, http.MethodPost, f.base+"/actions", map[string]any{
				"idempotency_key": "author-one", "name": "Run tests", "command": "go test ./...",
			}), test.write)
		})
	}
}
