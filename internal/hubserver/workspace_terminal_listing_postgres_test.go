package hubserver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type workspaceRecordingArchiveDetails struct {
	attachment.Storage
	root string
}

func (s workspaceRecordingArchiveDetails) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	if err := ctx.Err(); err != nil {
		return attachment.ArchiveReference{}, err
	}
	ref, compressed, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return ref, err
	}
	path := filepath.Join(s.root, ref.Key)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return ref, err
	}
	return ref, os.WriteFile(path, compressed, 0o600)
}

func (s workspaceRecordingArchiveDetails) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.Open(filepath.Join(s.root, key))
}

func (s workspaceRecordingArchiveDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	return attachment.ReadArchive(ctx, s, organization, ref)
}

func workspaceRecordingReader(t *testing.T, f hostedSecurityFixture, name, role string) (hostedSecurityUser, string) {
	t.Helper()
	now := f.service.config.now()
	identity := auth.Identity{Subject: name, Email: name + "@example.test", EmailVerified: true, Hosted: &auth.HostedIdentity{
		Subject: name, OrganizationID: f.service.config.Hosted.WorkOSOrganizationID, SessionID: "session_" + name,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}}
	f.provider.mu.Lock()
	f.provider.sessions[identity.Hosted.SessionID] = *identity.Hosted
	f.provider.mu.Unlock()
	membership, err := f.provider.CreateMembership(t.Context(), identity.Subject, identity.Hosted.OrganizationID, role)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.addHostedMember(t.Context(), identity, membership); err != nil {
		t.Fatal(err)
	}
	token, _, err := f.service.hostedSessions.CreateIdentitySession(t.Context(), identity)
	if err != nil {
		t.Fatal(err)
	}
	var principal string
	organization := tracker.OrganizationID(f.service.config.Hosted.OrganizationID)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), identity.Subject)
		if err != nil {
			return err
		}
		principal = member.PrincipalID
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: identity.Subject, ProjectID: f.project}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: member.PrincipalID, ProjectID: f.project})
	}); err != nil {
		t.Fatal(err)
	}
	return hostedSecurityUser{identity: identity, token: token}, principal
}

func TestPostgresWorkspaceRecordingListsFilterBeforeLimit(t *testing.T) {
	started := time.Now().UTC().Truncate(time.Microsecond)
	clock := started
	var clockMu sync.Mutex
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	details := workspaceRecordingArchiveDetails{root: t.TempDir()}
	bootstrap := newWorkspaceNativeService(t, Config{now: now, RunDetails: details, Workspace: &WorkspaceConfig{Enabled: true, maintenanceTicks: make(chan time.Time)}})
	admin := workspaceTestDB(t, bootstrap)
	organizations := []tracker.OrganizationID{"org_first", "org_first_second"}
	hubs := make([]*Service, 0, len(organizations))
	type savedWorkspace struct {
		fixture      hostedSecurityFixture
		member       hostedSecurityUser
		owner        hostedSecurityUser
		admin        hostedSecurityUser
		memberID     string
		ownerID      string
		workspace    string
		recording    string
		relaySession string
		artifact     string
		cast         string
		isolation    string
	}
	var saved []*savedWorkspace
	for _, organization := range organizations {
		hub := openWorkspaceTenantHub(t, bootstrap.config, admin, organization)
		hubs = append(hubs, hub)
		project := newNativeFixture(t, hub, organization, "Recording visibility project")
		fixture := hostedSecurityFixture{service: hub, provider: hub.config.Hosted.Provider.(*hostedSecurityProvider), project: project.project.ID, base: project.base}
		member, memberID := workspaceRecordingReader(t, fixture, "recording_member_"+string(organization), "member")
		owner, ownerID := workspaceRecordingReader(t, fixture, "recording_owner_"+string(organization), "owner")
		administrator, _ := workspaceRecordingReader(t, fixture, "recording_admin_"+string(organization), "admin")
		for index := range 2 {
			issue := project.create(t, fmt.Sprintf("Recording workspace %d", index))
			record := &savedWorkspace{fixture: fixture, member: member, owner: owner, admin: administrator, memberID: memberID, ownerID: ownerID, workspace: newNativeID("ws"), isolation: workspacesession.IsolationContainer}
			if index == 1 {
				record.isolation = workspacesession.IsolationUser
			}
			if err := hub.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.(postgres.WorkspaceWriter).CreateWorkspaceSession(t.Context(), postgres.WorkspaceCreate{
					OrganizationID: organization, ProjectID: fixture.project, ID: record.workspace, SubjectWorkItemID: string(issue.WorkItemID),
					Requires: []string{workspacesession.CapabilityTerminal}, IdleTimeoutSeconds: 600, ExpiresAt: started.Add(time.Hour), RequestedExpiresAt: started.Add(time.Minute), CreatedBy: memberID, CreatedAt: started,
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			saved = append(saved, record)
		}
	}
	clockMu.Lock()
	clock = started.Add(time.Second)
	clockMu.Unlock()
	for _, record := range saved {
		f := record.fixture
		organization := tracker.OrganizationID(f.service.config.Hosted.OrganizationID)
		record.relaySession = newNativeID("relaysession")
		otherRelay := newNativeID("relaysession")
		for _, session := range []struct {
			id        string
			principal string
			subject   string
		}{
			{id: record.relaySession, principal: record.memberID, subject: record.member.identity.Subject},
			{id: otherRelay, principal: record.ownerID, subject: record.owner.identity.Subject},
		} {
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.(workspaceWriter).CreateRelaySession(t.Context(), postgres.CreateRelaySessionInput{
					OrganizationID: organization, ProjectID: f.project, WorkspaceID: record.workspace, ID: session.id, PrincipalID: session.principal, Subject: session.subject,
					ConnectionID: newNativeID("relayconn"), Channels: []string{workspacesession.ChannelTerminal}, OpenedAt: started,
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		for sequence := range 101 {
			at := started.Add(time.Duration(sequence) * time.Microsecond)
			state := &terminalStreamState{
				id: newNativeID("termrec"), workspaceID: record.workspace, organizationID: string(organization), projectID: string(f.project),
				relaySessionID: otherRelay, streamID: workspacesession.StreamID("relayperson", sequence+1), principalID: record.ownerID, subject: record.owner.identity.Subject,
				isolation: workspacesession.IsolationUser, startedAt: at, cols: 80, rows: 24, recording: workspacesession.NewRecording(80, 24, at),
			}
			if sequence == 0 {
				state.relaySessionID, state.principalID, state.subject = record.relaySession, record.memberID, record.member.identity.Subject
				state.isolation = record.isolation
				record.recording = state.id
			}
			state.recording.Append(workspacesession.AsciicastOutput, fmt.Sprintf("recording %s %d", record.workspace, sequence), at)
			if err := f.service.workspaces.finishTerminalRecordingRecord(t.Context(), state); err != nil {
				t.Fatal(err)
			}
			if sequence == 0 {
				record.cast = state.recording.Cast()
			}
		}
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			audits, err := r.(workspaceReader).RecentRelaySessions(t.Context(), postgres.RelaySessionsInput{OrganizationID: organization, ProjectID: f.project, WorkspaceID: record.workspace, Limit: 2})
			if err != nil {
				return err
			}
			for _, audit := range audits {
				if audit.ID == record.relaySession {
					record.artifact = audit.RecordingArtifact
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if record.artifact == "" {
			t.Fatal("older recording has no durable relay artifact")
		}
	}
	verify := func(phase string) {
		t.Helper()
		for index, record := range saved {
			f := record.fixture
			path := f.base + "/workspaces/" + record.workspace + "/terminal-recordings"
			for _, request := range []struct {
				name  string
				user  hostedSecurityUser
				path  string
				admin bool
			}{
				{name: "own older recording", user: record.member, path: path},
				{name: "older relay artifact", user: record.owner, path: record.artifact},
				{name: "admin readable older recording", user: record.admin, path: path, admin: true},
				{name: "admin older relay artifact", user: record.admin, path: record.artifact, admin: true},
			} {
				response := f.request(t, request.user, http.MethodGet, request.path, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var listing struct {
					Recordings []TerminalRecording `json:"recordings"`
				}
				decodeHubResponse(t, response, &listing)
				if request.admin && record.isolation == workspacesession.IsolationUser {
					if len(listing.Recordings) != 0 {
						t.Errorf("%s %s exposed user-isolation recordings: %+v", phase, request.name, listing.Recordings)
					}
					continue
				}
				if len(listing.Recordings) != 1 || listing.Recordings[0].ID != record.recording {
					t.Errorf("%s %s workspace %s recordings=%+v, want older recording %s", phase, request.name, record.workspace, listing.Recordings, record.recording)
				}
			}
			response := f.request(t, record.member, http.MethodGet, path+"/"+record.recording, nil)
			requireNativeStatus(t, response, http.StatusOK)
			if response.Body.String() != record.cast {
				t.Errorf("%s older recording bytes differ after archive read", phase)
			}
			adminStatus := http.StatusOK
			if record.isolation == workspacesession.IsolationUser {
				adminStatus = http.StatusNotFound
			}
			response = f.request(t, record.admin, http.MethodGet, path+"/"+record.recording, nil)
			requireNativeStatus(t, response, adminStatus)
			if adminStatus == http.StatusOK && response.Body.String() != record.cast {
				t.Errorf("%s admin recording bytes differ after archive read", phase)
			}
			requireNativeStatus(t, f.request(t, hostedSecurityUser{}, http.MethodGet, path, nil), http.StatusUnauthorized)
			foreign := saved[(index+2)%len(saved)]
			foreignPath := foreign.fixture.base + "/workspaces/" + foreign.workspace + "/terminal-recordings"
			requireNativeStatus(t, f.request(t, record.member, http.MethodGet, foreignPath, nil), http.StatusNotFound)
			requireNativeStatus(t, f.request(t, record.member, http.MethodGet, foreignPath+"/"+foreign.recording, nil), http.StatusNotFound)
			otherWorkspace := saved[index^1]
			requireNativeStatus(t, f.request(t, record.member, http.MethodGet, f.base+"/workspaces/"+otherWorkspace.workspace+"/terminal-recordings/"+record.recording, nil), http.StatusNotFound)
		}
	}
	verify("before reopen")
	for _, hub := range hubs {
		reopened := reopenWorkspaceHub(t, hub)
		for _, record := range saved {
			if string(record.fixture.service.localOrganization) == string(reopened.localOrganization) {
				record.fixture.service = reopened
			}
		}
	}
	verify("after reopen")
}
