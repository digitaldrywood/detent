package cloudentry

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/cloudentry/tenant"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

type TenantSpec = tenant.TenantSpec

type Launcher = tenant.Launcher

func (s *Service) hostedTenantConfig(ctx context.Context, spec TenantSpec, configure func(TenantSpec) ([]byte, error)) (hubserver.Config, error) {
	org := spec.Organization
	var configurationKey string
	if org.HostedConfigurationKey != nil {
		spec.Directory = ""
		configurationKey = *org.HostedConfigurationKey
		imported, err := s.readEntryObject(ctx, configurationKey)
		if err != nil {
			return hubserver.Config{}, err
		}
		spec.ImportedConfiguration = imported
	}
	raw, err := configure(spec)
	if err != nil {
		return hubserver.Config{}, err
	}
	cfg, err := s.config.SharedHub.TenantConfig(raw, spec)
	if err != nil {
		return hubserver.Config{}, err
	}
	var workspaceKey string
	if org.WorkspacePolicyKey != nil {
		workspaceKey = *org.WorkspacePolicyKey
	}
	switch {
	case workspaceKey != "":
		data, err := s.readEntryObject(ctx, workspaceKey)
		if err != nil {
			return hubserver.Config{}, err
		}
		var workspace hubserver.WorkspaceConfig
		if err := json.Unmarshal(data, &workspace); err != nil {
			return hubserver.Config{}, err
		}
		if !workspace.Enabled {
			return hubserver.Config{}, errors.New("stored workspace policy is invalid")
		}
		if err := workspace.Normalized().Validate(); err != nil {
			return hubserver.Config{}, err
		}
		cfg.Workspace = &workspace
	case org.WorkspacePolicyKey != nil && org.HostedConfigurationKey != nil:
		cfg.Workspace = nil
	case cfg.Workspace != nil:
		if err := cfg.Workspace.Normalized().Validate(); err != nil {
			return hubserver.Config{}, err
		}
		data, err := json.Marshal(cfg.Workspace)
		if err != nil {
			return hubserver.Config{}, err
		}
		workspaceKey, err = s.storeEntryObject(ctx, data)
		if err != nil {
			return hubserver.Config{}, err
		}
	}
	if org.HostedConfigurationKey != nil && org.WorkspacePolicyKey != nil {
		return cfg, nil
	}
	if org.HostedConfigurationKey == nil && len(raw) > 0 {
		configurationKey, err = s.storeEntryObject(ctx, raw)
		if err != nil {
			return hubserver.Config{}, err
		}
	}
	result, err := s.registry.store.db.ExecContext(ctx, `UPDATE platform.organizations
SET workspace_policy_key=$1,hosted_configuration_key=$2
WHERE id=$3 AND provider_id=$4 AND generation=$5 AND state IN ('allocating','ready')
AND (workspace_policy_key IS NOT DISTINCT FROM $6::text OR workspace_policy_key=$1)
AND (hosted_configuration_key IS NULL OR hosted_configuration_key=$2)`, workspaceKey, configurationKey, org.ID, org.ProviderID, org.Generation, org.WorkspacePolicyKey)
	if err != nil {
		return hubserver.Config{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return hubserver.Config{}, err
	}
	if count != 1 {
		return hubserver.Config{}, ErrOrganizationConflict
	}
	return cfg, nil
}
