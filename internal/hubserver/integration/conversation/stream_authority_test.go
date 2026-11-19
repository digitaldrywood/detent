package conversation_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConversationPostgresStreamReauthorizesAgainstTenantProject(t *testing.T) {
	t.Parallel()
	hub := hubfixture.Open(t, hubfixture.Config{Conversation: &conversations.ConversationConfig{Enabled: true}})
	peer := hub.CreateOrganization(t, "Independent stream organization")
	server := httptest.NewServer(hub.Handler())
	t.Cleanup(server.Close)

	type reader struct {
		project hubfixture.Native
		key     hubtest.SeededCredential
		target  streamTarget
		stream  *conversationEventStream
	}
	open := func(organization tracker.OrganizationID) reader {
		project := hubfixture.NewNativeWith(t, hub, organization, hubtest.ProjectOptions{ID: "project_stream", Name: "Stream project"})
		writer, err := hub.Credential(t, organization, project.Token)
		if err != nil {
			t.Fatal(err)
		}
		target := seedStreamConversation(t, hub, organization, project.Project.ID, writer.ID, conversation.VisibilityShared)
		key := hubtest.Credential(t, hub.Repository, organization, hubtest.CredentialOptions{
			Scope: "admin", Projects: []tracker.ProjectID{project.Project.ID}, Now: hub.Now(),
		})
		stream := openConversationEventStream(t, server, target.path+"/events?after=0", func(request *http.Request) {
			request.Header.Set("Authorization", "Bearer "+key.Token)
		})
		stream.requireUpdate(t, "ready", target.ready)
		return reader{project: project, key: key, target: target, stream: stream}
	}
	first, other := open(hub.Primary), open(peer)
	if err := hub.Repository.WriteTenant(t.Context(), hub.Primary, corestore.Serving, func(writer tenantstore.Writer) error {
		return writer.RevokeAccessCredential(t.Context(), first.key.ID, hub.Now())
	}); err != nil {
		t.Fatal(err)
	}

	patch := func(active reader, title string) {
		t.Helper()
		response := active.project.Request(t, http.MethodPatch, active.target.path, conversations.ConversationPatchRequest{Title: &title})
		hubfixture.RequireStatus(t, response, http.StatusOK)
	}
	patch(first, "Protected after native revocation")
	first.stream.requireClosed(t, conversations.ConversationClosedAccessRevoked)
	patch(other, "Independent organization remains readable")
	other.stream.requireUpdate(t, "Independent organization remains readable", 0)
}

func TestConversationStreamRefreshesEntryKeyAuthorityPostgres(t *testing.T) {
	t.Parallel()
	clock := hubtest.NewClock(time.Now())
	authority := &streamKeyAuthority{}
	hub := hubfixture.OpenConversationKeyStreamEntry(t, clock.Now, authority.resolve)
	authority.hub = hub
	removed, retained := tracker.ProjectID("project_removed"), tracker.ProjectID("project_retained")
	hub.SeedProject(t, removed, "Revocable project")
	hub.SeedProject(t, retained, "Retained project")

	for _, test := range []struct {
		name   string
		change string
		replay bool
		want   string
	}{
		{name: "revoked", change: "revoke", want: conversations.ConversationClosedAccessRevoked},
		{name: "narrowed", change: "narrow", want: conversations.ConversationClosedAccessRevoked},
		{name: "unavailable", change: "unavailable", want: conversations.ConversationClosedServerError},
		{name: "replay_revoked", change: "revoke", replay: true, want: conversations.ConversationClosedAccessRevoked},
		{name: "replay_narrowed", change: "narrow", replay: true, want: conversations.ConversationClosedAccessRevoked},
	} {
		t.Run(test.name, func(t *testing.T) {
			owner, key, principal := seedStreamKey(t, hub, test.name, removed, retained)
			target := seedStreamConversation(t, hub.Hub, hub.Primary, removed, principal, conversation.VisibilityPrivate)
			keep := seedStreamConversation(t, hub.Hub, hub.Primary, retained, principal, conversation.VisibilityPrivate)
			release := func() {}
			handler := hub.Handler()
			if test.replay {
				target.append(t, "Protected backlog from before the authority changed", 0)
				handler, release = pauseConversationReplay(handler, target.path+"/events", target.ready)
				t.Cleanup(release)
			}
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			stream := openKeyEventStream(t, server, hub, target, owner, key)
			stream.requireUpdate(t, "ready", target.ready)
			kept := openKeyEventStream(t, server, hub, keep, owner, key)
			kept.requireUpdate(t, "ready", keep.ready)

			checkPeer := func() {}
			if test.name == "narrowed" {
				otherAuthority := &streamKeyAuthority{}
				peer := hub.OpenConversationKeyStreamPeer(t, "org_stream_peer", otherAuthority.resolve)
				otherAuthority.hub = peer
				peer.SeedProject(t, removed, "Independent project")
				otherOwner, otherKey, otherPrincipal := seedStreamKey(t, peer, "peer", removed)
				otherTarget := seedStreamConversation(t, peer.Hub, peer.Primary, removed, otherPrincipal, conversation.VisibilityPrivate)
				otherServer := httptest.NewServer(peer.Handler())
				t.Cleanup(otherServer.Close)
				other := openKeyEventStream(t, otherServer, peer, otherTarget, otherOwner, otherKey)
				other.requireUpdate(t, "ready", otherTarget.ready)
				checkPeer = func() {
					patchStreamTitle(t, peer, otherOwner, otherTarget, "Independent tenant live update")
					other.requireUpdate(t, "Independent tenant live update", 0)
				}
			}

			switch test.change {
			case "revoke":
				target.write(t, func(writer tenantstore.Writer) error {
					return writer.RevokeAccessCredential(t.Context(), key.ID, hub.Now())
				})
			case "narrow":
				target.write(t, func(writer tenantstore.Writer) error {
					return writer.DeleteAccessTokenGrant(t.Context(), key.ID, target.project)
				})
			case "unavailable":
				authority.unavailable.Store(true)
				t.Cleanup(func() { authority.unavailable.Store(false) })
			}
			if test.replay {
				clock.Advance(conversations.ConversationAuthorizeInterval)
				release()
			} else {
				patchStreamTitle(t, hub, owner, target, "Protected live update")
			}
			stream.requireClosed(t, test.want)

			patchStreamTitle(t, hub, owner, keep, "Retained project live update")
			if test.change == "narrow" {
				kept.requireUpdate(t, "Retained project live update", 0)
			} else {
				kept.requireClosed(t, test.want)
			}
			checkPeer()
		})
	}

	t.Run("replay_across_pages_then_follow", func(t *testing.T) {
		owner, key, principal := seedStreamKey(t, hub, "resume", retained)
		target := seedStreamConversation(t, hub.Hub, hub.Primary, retained, principal, conversation.VisibilityPrivate)
		last := target.append(t, "Retained event beyond the replay page", 500)
		server := httptest.NewServer(hub.Handler())
		t.Cleanup(server.Close)
		stream := openKeyEventStream(t, server, hub, target, owner, key)
		stream.requireUpdate(t, "ready", target.ready)
		for {
			frame := stream.next(t)
			if frame.kind != conversation.EventConversationUpdated {
				t.Fatalf("replay stopped before %d: %+v", last, frame)
			}
			if frame.cursor == last {
				frame.requireTitle(t, "Retained event beyond the replay page")
				break
			}
		}
		patchStreamTitle(t, hub, owner, target, "Live event after replay")
		stream.requireUpdate(t, "Live event after replay", 0)
	})
}

type streamKeyAuthority struct {
	hub         *hubfixture.ConversationHostedEntry
	unavailable atomic.Bool
}

func (a *streamKeyAuthority) resolve(ctx context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
	if a.unavailable.Load() {
		return cloudassert.Claims{}, operatortool.ErrServiceUnavailable
	}
	key := apikey.KeyAuthority{
		ID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Kind: "personal", Permission: apikey.ScopeRead,
		ProjectContext: apikey.ProjectContext{Access: "selected"},
	}
	if err := a.hub.Repository.ReadTenant(ctx, a.hub.Primary, corestore.Serving, func(reader tenantstore.Reader) error {
		stored, err := reader.AccessCredential(ctx, origin.KeyID)
		if err != nil {
			return err
		}
		if stored.RevokedAt != nil || stored.UserID != origin.Subject {
			return operatortool.ErrAccessDenied
		}
		grants, err := reader.AccessTokenGrants(ctx, origin.KeyID, "", 10)
		for _, grant := range grants {
			key.Projects = append(key.Projects, string(grant.ProjectID))
		}
		return err
	}); err != nil {
		return cloudassert.Claims{}, err
	}
	now := a.hub.Now()
	return cloudassert.Claims{
		Issuer: origin.Issuer, Audience: string(a.hub.Primary), Generation: origin.Generation, Kind: cloudassert.KindKey,
		ProviderOrganization: a.hub.Config.Hosted.WorkOSOrganizationID, Subject: origin.Subject, Role: "owner", Key: &key,
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}, nil
}

func seedStreamKey(t *testing.T, hub *hubfixture.ConversationHostedEntry, label string, projects ...tracker.ProjectID) (hubfixture.HostedUser, apikey.KeyAuthority, string) {
	t.Helper()
	user := hub.User(t, "stream-"+label, "owner", label+"@example.test", "", "")
	var member accessstore.Member
	if err := hub.Repository.ReadTenant(t.Context(), hub.Primary, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		member, err = reader.AccessMember(t.Context(), user.Identity.Subject)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, project := range projects {
		hub.Grant(t, user, project, true, false)
	}
	stored := hubtest.Credential(t, hub.Repository, hub.Primary, hubtest.CredentialOptions{
		Projects: projects, Now: hub.Now(), KeyScope: "read", Member: &hubtest.SeededMember{Member: member},
	})
	key := apikey.KeyAuthority{
		ID: stored.ID, CredentialBinding: stored.Hash, Kind: "personal", Permission: apikey.ScopeRead,
		ProjectContext: apikey.ProjectContext{Access: "selected"},
	}
	for _, project := range projects {
		key.Projects = append(key.Projects, string(project))
	}
	return user, key, member.PrincipalID
}

type streamTarget struct {
	hub          *hubfixture.Hub
	organization tracker.OrganizationID
	project      tracker.ProjectID
	id           string
	path         string
	ready        int64
}

func seedStreamConversation(t *testing.T, hub *hubfixture.Hub, organization tracker.OrganizationID, project tracker.ProjectID, principal string, visibility conversation.Visibility) streamTarget {
	t.Helper()
	id := conversation.NewConversationID()
	target := streamTarget{hub: hub, organization: organization, project: project, id: id, path: hubfixture.ProjectBase(organization, project) + "/conversations/" + id}
	now := hub.Now().UTC().Truncate(time.Microsecond)
	target.write(t, func(writer tenantstore.Writer) error {
		_, err := writer.CreateConversation(t.Context(), conversationstore.Conversation{
			OrganizationID: organization, ProjectID: project, ID: id, OwnerPrincipalID: principal, Origin: conversations.ConversationOriginUser,
			Title: "Stream conversation", Visibility: visibility, Status: conversation.StatusActive, Revision: 1,
			Execution:   conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: now},
			Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now,
		})
		return err
	})
	target.ready = target.append(t, "ready", 0)
	return target
}

func (target streamTarget) write(t *testing.T, action func(tenantstore.Writer) error) {
	t.Helper()
	if err := target.hub.Repository.WriteTenant(t.Context(), target.organization, corestore.Serving, action); err != nil {
		t.Fatal(err)
	}
}

func (target streamTarget) append(t *testing.T, title string, earlier int) int64 {
	t.Helper()
	var cursor int64
	target.write(t, func(writer tenantstore.Writer) error {
		now := target.hub.Now().UTC().Truncate(time.Microsecond)
		appendEvent := func(text string) (int64, error) {
			payload, err := json.Marshal(map[string]string{"title": text})
			if err != nil {
				return 0, err
			}
			event, err := writer.AppendConversationEvent(t.Context(), conversationstore.Event{
				OrganizationID: target.organization, ConversationID: target.id,
				Event: conversation.Event{Type: conversation.EventConversationUpdated, Data: payload, CreatedAt: now},
			})
			return event.Seq, err
		}
		for range earlier {
			if _, err := appendEvent("Earlier retained update"); err != nil {
				return err
			}
		}
		var err error
		cursor, err = appendEvent(title)
		return err
	})
	return cursor
}

func patchStreamTitle(t *testing.T, hub *hubfixture.ConversationHostedEntry, user hubfixture.HostedUser, target streamTarget, title string) {
	t.Helper()
	response := hub.BrowserRequest(t, user, "owner", http.MethodPatch, target.path, conversations.ConversationPatchRequest{Title: &title})
	hubfixture.RequireStatus(t, response, http.StatusOK)
}

func openKeyEventStream(t *testing.T, server *httptest.Server, hub *hubfixture.ConversationHostedEntry, target streamTarget, user hubfixture.HostedUser, key apikey.KeyAuthority) *conversationEventStream {
	t.Helper()
	return openConversationEventStream(t, server, target.path+"/events", func(request *http.Request) {
		hub.SignConversationKeyStream(t, request, user, key)
	})
}

type conversationEventStream struct {
	reader *bufio.Reader
}

type conversationEventFrame struct {
	cursor int64
	kind   conversation.EventType
	data   string
}

func openConversationEventStream(t *testing.T, server *httptest.Server, path string, authenticate func(*http.Request)) *conversationEventStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	authenticate(request)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("open %s: %s %s", path, response.Status, body)
	}
	return &conversationEventStream{reader: bufio.NewReader(response.Body)}
}

func (stream *conversationEventStream) next(t *testing.T) conversationEventFrame {
	t.Helper()
	var frame conversationEventFrame
	for {
		line, err := stream.reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read conversation event: %v", err)
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			frame.cursor, err = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id: ")), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
		case strings.HasPrefix(line, "event: "):
			frame.kind = conversation.EventType(strings.TrimSpace(strings.TrimPrefix(line, "event: ")))
		case strings.HasPrefix(line, "data: "):
			frame.data = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		case line == "\n":
			if frame.kind == conversation.EventHeartbeat {
				frame = conversationEventFrame{}
			} else {
				return frame
			}
		}
	}
}

func (frame conversationEventFrame) requireTitle(t *testing.T, title string) {
	t.Helper()
	var payload struct{ Title string }
	if err := json.Unmarshal([]byte(frame.data), &payload); err != nil || payload.Title != title {
		t.Fatalf("event %d title = %q, want %q, payload %s, error %v", frame.cursor, payload.Title, title, frame.data, err)
	}
}

func (stream *conversationEventStream) requireUpdate(t *testing.T, title string, cursor int64) {
	t.Helper()
	frame := stream.next(t)
	if frame.kind != conversation.EventConversationUpdated || cursor != 0 && frame.cursor != cursor {
		t.Fatalf("expected conversation update, got %+v", frame)
	}
	frame.requireTitle(t, title)
}

func (stream *conversationEventStream) requireClosed(t *testing.T, reason string) {
	t.Helper()
	frame := stream.next(t)
	var payload struct{ Reason string }
	if err := json.Unmarshal([]byte(frame.data), &payload); err != nil || frame.kind != conversation.EventClosed || payload.Reason != reason {
		t.Fatalf("expected stream close for %q, got %+v, error %v", reason, frame, err)
	}
	if trailing, err := io.ReadAll(stream.reader); err != nil || len(trailing) != 0 {
		t.Fatalf("stream did not close cleanly: %q, %v", trailing, err)
	}
}

type conversationReplayGate struct {
	http.ResponseWriter
	ctx     context.Context
	first   string
	release <-chan struct{}
	pause   bool
}

func (gate *conversationReplayGate) Write(data []byte) (int, error) {
	gate.pause = strings.HasPrefix(string(data), gate.first)
	return gate.ResponseWriter.Write(data)
}

func (gate *conversationReplayGate) Flush() {
	gate.ResponseWriter.(http.Flusher).Flush()
	if gate.pause {
		gate.pause = false
		select {
		case <-gate.release:
		case <-gate.ctx.Done():
		}
	}
}

func pauseConversationReplay(handler http.Handler, path string, cursor int64) (http.Handler, func()) {
	release := make(chan struct{})
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == path {
			response = &conversationReplayGate{
				ResponseWriter: response, ctx: request.Context(), release: release,
				first: "id: " + strconv.FormatInt(cursor, 10) + "\n",
			}
		}
		handler.ServeHTTP(response, request)
	}), sync.OnceFunc(func() { close(release) })
}
