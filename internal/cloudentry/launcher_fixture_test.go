package cloudentry

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

type silentLauncher struct {
	mu       sync.Mutex
	err      error
	failure  error
	running  map[string]bool
	launches int
	stops    int
}

func (l *silentLauncher) Start(_ context.Context, spec TenantSpec) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil || l.running[spec.Organization.ID] {
		return l.err
	}
	l.running[spec.Organization.ID] = true
	l.launches++
	return nil
}

func (l *silentLauncher) WaitReady(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running[id] {
		return errors.New("tenant fixture is not running")
	}
	return nil
}

func (l *silentLauncher) Stop(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[id] {
		l.stops++
	}
	delete(l.running, id)
	return nil
}

func (l *silentLauncher) Close() error { return nil }

func (l *silentLauncher) Failure(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running[id] {
		return nil
	}
	return l.failure
}

func (l *silentLauncher) counts() (launches, stops, running int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.launches, l.stops, len(l.running)
}

func TestLegacyWorkspacePolicySurvivesRestart(t *testing.T) {
	f := newEntryFixture(t)
	for _, id := range []string{"org_alpha", "org_beta"} {
		org, err := f.service.registry.Organization(t.Context(), id)
		if err != nil || org.WorkspacePolicyKey != nil || org.HostedConfigurationKey != nil {
			t.Fatalf("registration %s did not leave migration pending: %v", id, err)
		}
	}
	if _, err := f.database.ExecContext(t.Context(), "UPDATE platform.organizations SET workspace_policy_key='' WHERE id='org_alpha'"); err != nil {
		t.Fatal(err)
	}
	policies := map[string]*hubserver.WorkspaceConfig{
		"org_alpha": {Enabled: true, PlanMaxOpen: 5, PersonMaxOpen: 2, FilesDeny: []string{"private/**"}},
		"org_beta":  nil,
	}
	config := f.service.config
	config.SharedHub = &SharedHubConfig{TenantConfig: func(_ []byte, spec TenantSpec) (hubserver.Config, error) {
		if spec.Directory == "" {
			return hubserver.Config{}, nil
		}
		return hubserver.Config{Workspace: policies[spec.Organization.ID]}, nil
	}}
	for pass := range 2 {
		entry := &Service{config: config, registry: f.service.registry, attachments: f.service.attachments}
		for _, id := range []string{"org_alpha", "org_beta"} {
			org, err := entry.registry.Organization(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			spec := entry.tenantSpec(org)
			if pass == 0 && spec.Directory != filepath.Join(config.StateDir, "tenants", id) {
				t.Fatalf("missing registration-only migration directory for %s", id)
			}
			settings, err := entry.hostedTenantConfig(t.Context(), spec, func(s TenantSpec) ([]byte, error) {
				if pass == 1 {
					if s.Directory != "" || string(s.ImportedConfiguration) != id {
						t.Fatal("restarted host lost the imported organization configuration")
					}
					return s.ImportedConfiguration, nil
				}
				return []byte(id), nil
			})
			if err != nil || !reflect.DeepEqual(settings.Workspace, policies[id]) {
				t.Fatalf("pass %d, %s workspace = %+v error=%v", pass, id, settings.Workspace, err)
			}
			saved, err := entry.registry.Organization(t.Context(), id)
			if err != nil || saved.HostedConfigurationKey == nil || saved.WorkspacePolicyKey == nil {
				t.Fatalf("incomplete %s migration: %v", id, err)
			}
			if policies[id] == nil {
				if *saved.WorkspacePolicyKey != "" {
					t.Fatal("legacy reader would enable a disabled workspace")
				}
			} else {
				data, err := entry.readEntryObject(t.Context(), *saved.WorkspacePolicyKey)
				var prior hubserver.WorkspaceConfig
				if err != nil || json.Unmarshal(data, &prior) != nil || !reflect.DeepEqual(&prior, policies[id]) {
					t.Fatal("legacy reader lost workspace policy")
				}
			}

		}
	}
}
