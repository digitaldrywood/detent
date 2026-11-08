package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConfigurationRankOwnerScopesRevisionsAndReopens(t *testing.T) {
	var config hubserver.Config
	service, _ := clihangPostgresHub(t, "rank-token", nil, clihangPostgresOptions{config: &config, seed: func(repository *postgres.Repository) error {
		for _, org := range []tracker.OrganizationID{"org_example", "org_other"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				now := time.Now().UTC()
				user, member, admin := "user_"+string(org), "membership_"+string(org), "admin_"+string(org)
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "read_" + string(org), Hash: apikey.HashToken("rank-read-" + string(org)), Name: "Example read key", Scope: "operator", KeyScope: "read", ProjectAccess: "all", Kind: "personal", UserID: user, MembershipID: member, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "selected_" + string(org), Hash: apikey.HashToken("rank-selected-" + string(org)), Name: "Example selected read key", Scope: "operator", KeyScope: "read", ProjectAccess: "selected", Kind: "personal", UserID: user, MembershipID: member, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				for _, project := range []tracker.ProjectID{"prj_rank_first", "prj_rank_second"} {
					if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Slug: strings.ReplaceAll(string(project), "_", "-"), Profile: "native", CreatedAt: now}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}); err != nil {
						return err
					}
					if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, ProjectID: project, UserID: user, CanWrite: true}); err != nil {
						return err
					}
					if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, ProjectID: project, TokenID: admin}); err != nil {
						return err
					}
					if project == "prj_rank_first" {
						if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, ProjectID: project, TokenID: "selected_" + string(org)}); err != nil {
							return err
						}
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}})
	open := func(org string) (*hubserver.Service, *httptest.Server) {
		t.Helper()
		hosted := *config.Hosted
		hosted.OrganizationID, hosted.WorkOSOrganizationID = org, "provider_"+org
		config.Hosted = &hosted
		current, err := hubserver.Open(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := current.Close(); err != nil {
				t.Error(err)
			}
		})
		server := httptest.NewServer(current.Handler())
		t.Cleanup(server.Close)
		return current, server
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	owner := func(server *httptest.Server, org, token string) *hubConfigurationRank {
		t.Helper()
		value, err := newHubConfigurationRank(globalconfig.HubClient{URL: server.URL, OrganizationID: org, TokenEnvironment: "EXAMPLE_RANK_TOKEN"}, func(string) string { return token })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(value.Close)
		return value
	}
	expected := make(map[string]projectsettings.Rank)
	for index, org := range []string{"org_example", "org_other"} {
		current, server := open(org)
		credential := "rank-token"
		if index > 0 {
			credential += "-other"
		}
		rank := owner(server, org, credential)
		before, err := rank.Read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(before.ProjectIDs) != 3 || !slices.Contains(before.ProjectIDs, "prj_rank_first") || !slices.Contains(before.ProjectIDs, "prj_rank_second") {
			t.Fatalf("rank projects=%+v", before)
		}
		alias, selected := "first", tracker.ProjectID("prj_rank_first")
		if index > 0 {
			alias, selected = "second", "prj_rank_second"
		}
		ids := []tracker.ProjectID{selected}
		for _, id := range before.ProjectIDs {
			if id != selected {
				ids = append(ids, id)
			}
		}
		cfg := globalconfig.Config{Client: globalconfig.HubClient{URL: server.URL, OrganizationID: org, TokenEnvironment: "EXAMPLE_RANK_TOKEN", NativeProjects: map[string]string{"first": "prj_rank_first", "second": "prj_rank_second"}}, Projects: []globalconfig.Project{{ID: "first"}, {ID: "second"}}}
		opts := defaultOptions()
		opts.resolvePath = func(string) (globalconfig.PathResolution, error) {
			return globalconfig.PathResolution{Path: "global.yaml"}, nil
		}
		opts.readOrDefault = func(string) (globalconfig.Config, error) {
			current := cfg
			current.Projects = slices.Clone(cfg.Projects)
			return current, nil
		}
		opts.lookupEnv = func(string) string { return credential }
		opts.write = func(string, globalconfig.Config) error {
			return errors.New("rank command attempted a local configuration write")
		}
		var signal Signal
		opts.signal = func(_ context.Context, current Signal) error {
			signal = current
			return nil
		}
		project, err := updateProject(t.Context(), "global.yaml", opts, OperationPromoteProject, alias, func(project *globalconfig.Project) error {
			project.Priority = 1
			return nil
		})
		if err != nil || project.Priority != 0 || signal.Operation != OperationPromoteProject || signal.ProjectID != alias || signal.Project.Priority != 0 {
			t.Fatalf("promoted id=%s priority=%d signal=%s/%s/%d error=%v", project.ID, project.Priority, signal.Operation, signal.ProjectID, signal.Project.Priority, err)
		}
		change := projectsettings.RankChange{ExpectedRevision: before.Revision, ProjectIDs: ids}
		after, err := rank.Read(t.Context())
		if err != nil || after.Revision != before.Revision+1 || !slices.Equal(after.ProjectIDs, ids) {
			t.Fatalf("applied rank=%+v error=%v", after, err)
		}
		expected[org] = after
		if _, err := rank.Apply(t.Context(), change); !configurationRankStatus(err, http.StatusConflict) {
			t.Fatalf("stale rank change: %v", err)
		}
		readOnly := owner(server, org, "rank-read-"+org)
		if value, err := readOnly.Read(t.Context()); err != nil || value.Revision != after.Revision || !slices.Equal(value.ProjectIDs, after.ProjectIDs) {
			t.Fatalf("authorized rank read=%+v error=%v", value, err)
		}
		selectedRead := owner(server, org, "rank-selected-"+org)
		if value, err := selectedRead.Read(t.Context()); err != nil || value.Revision != after.Revision || !slices.Equal(value.ProjectIDs, []tracker.ProjectID{"prj_rank_first"}) {
			t.Fatalf("selected rank read=%+v error=%v", value, err)
		}
		if _, err := readOnly.Apply(t.Context(), projectsettings.RankChange{ExpectedRevision: after.Revision, ProjectIDs: before.ProjectIDs}); !configurationRankStatus(err, http.StatusForbidden) {
			t.Fatalf("read key changed rank: %v", err)
		}
		foreignOrganization := "org_other"
		if org == "org_other" {
			foreignOrganization = "org_example"
		}
		foreign := owner(server, foreignOrganization, credential)
		if _, err := foreign.Read(t.Context()); !configurationRankStatus(err, http.StatusNotFound) {
			t.Fatalf("foreign rank read: %v", err)
		}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+rank.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		transport := &http.Transport{}
		t.Cleanup(transport.CloseIdleConnections)
		response, err := (&http.Client{Transport: transport}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated rank read=%d", response.StatusCode)
		}
		server.Close()
		if err := current.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for index, org := range []string{"org_example", "org_other"} {
		current, server := open(org)
		credential := "rank-token"
		if index > 0 {
			credential += "-other"
		}
		value, err := owner(server, org, credential).Read(t.Context())
		if err != nil || value.Revision != expected[org].Revision || !slices.Equal(value.ProjectIDs, expected[org].ProjectIDs) {
			t.Fatalf("reopened rank=%+v expected=%+v error=%v", value, expected[org], err)
		}
		server.Close()
		if err := current.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func configurationRankStatus(err error, status int) bool {
	var failure *api.APIError
	return errors.As(err, &failure) && failure.Status == status
}
