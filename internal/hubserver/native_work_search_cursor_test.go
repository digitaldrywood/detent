package hubserver

import (
	"errors"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSearchCursorSurvivesHubRestart(t *testing.T) {
	t.Parallel()
	admin := postgresStartupTemplate.Database(t)
	config := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	config.Hosted = nil
	config.ListenAddress = "127.0.0.1:0"
	config.Logger = discardLogger()
	config.InitialAdminToken = []byte(testHubAdminToken)
	config.RunDetails = &memoryRunDetails{}
	config.SecretKeys = secretTestKeys(t, "1", "1")
	before, err := Open(t.Context(), config)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := before.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	org := before.accessOrganization()
	other := tracker.OrganizationID("org_search_restart_other")
	seedWorkItemRecordOrganization(t, before.config.TenantRepository, before, nativeOrganization{ID: other, Name: string(other), Slug: string(other)})
	f := newNativeFixture(t, before, org, "Example project")
	for _, name := range []string{"restartterm alpha", "restartterm beta", "restartterm gamma"} {
		f.create(t, name)
	}
	repository := tenantstore.SearchRepository{Repository: before.config.TenantRepository}
	for {
		more, err := before.searchContent().RefreshOne(t.Context(), repository, org)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	query := searchstore.Query{Kind: "work", ProjectID: f.project.ID, Text: "restartterm", Limit: 2}
	first, err := before.searchContent().Find(t.Context(), repository, org, query)
	if err != nil || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page=%+v error=%v", first.Items, err)
	}
	if err := before.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}

	restartConfig := before.config
	restartConfig.TenantRepository, restartConfig.AccessRepository, restartConfig.BillingRepository = nil, nil, nil
	restartConfig.SecretKeys = secretTestKeys(t, "1", "1")
	after, err := Open(t.Context(), restartConfig)
	if err != nil {
		t.Fatalf("restart Hub: %v", err)
	}
	t.Cleanup(func() {
		if err := after.Close(); err != nil {
			t.Fatalf("close restarted Hub: %v", err)
		}
	})
	repository = tenantstore.SearchRepository{Repository: after.config.TenantRepository}
	query.Cursor = first.NextCursor
	second, err := after.searchContent().Find(t.Context(), repository, org, query)
	if err != nil || len(second.Items) != 1 || second.NextCursor != "" {
		t.Fatalf("page after restart=%+v cursor=%q error=%v", second.Items, second.NextCursor, err)
	}
	var titles []string
	for _, hit := range append(first.Items, second.Items...) {
		titles = append(titles, hit.Document.Title)
	}
	slices.Sort(titles)
	if !slices.Equal(titles, []string{"restartterm alpha", "restartterm beta", "restartterm gamma"}) {
		t.Fatalf("titles across restart=%v", titles)
	}
	if _, err := after.searchContent().Find(t.Context(), repository, other, query); !errors.Is(err, corestore.ErrInvalidCursor) {
		t.Fatalf("cursor issued to %s accepted by %s: %v", org, other, err)
	}
}
