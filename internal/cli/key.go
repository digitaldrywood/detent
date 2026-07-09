package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/apikey"
	storepkg "github.com/digitaldrywood/detent/internal/store"
)

func newKeyCommand(configPath *string, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "key",
		Short:   "Manage API keys",
		Example: `detent key add --name "Video Studio" --scope write --project digitaldrywood-video`,
	}
	cmd.AddCommand(
		newKeyAddCommand(configPath, opts),
		newKeyListCommand(configPath, opts),
		newKeyRotateCommand(configPath, opts),
		newKeyRevokeCommand(configPath, opts),
	)
	return cmd
}

func newKeyAddCommand(configPath *string, opts options) *cobra.Command {
	var name string
	var scope string
	var projects []string
	var expiresIn string
	cmd := &cobra.Command{
		Use:     "add",
		Short:   "Create an API key",
		Example: `detent key add --name "Video Studio" --scope write --project digitaldrywood-video --expires-in 90d`,
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			result, err := runKeyAdd(cmd.Context(), *configPath, opts, apikey.CreateRequest{
				Name:       name,
				Scopes:     []string{scope},
				ProjectIDs: projects,
				ExpiresIn:  expiresIn,
			})
			if err != nil {
				return cliAPIKeyError(err)
			}
			return out.Write(func(out io.Writer) error {
				_, err := fmt.Fprintf(out, "created api key %s\nname: %s\nkey: %s\nscope: %s\nprojects: %s\nexpires: %s\ntoken: %s\n",
					result.Key.ID,
					result.Key.Name,
					result.Key.Key,
					result.Key.Scope,
					apiKeyProjectText(result.Key.ProjectIDs),
					result.Key.ExpiresAt,
					result.Token,
				)
				return err
			}, result)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "human-readable key name")
	cmd.Flags().StringVar(&scope, "scope", string(apikey.ScopeWrite), "key scope: read, write, or admin")
	cmd.Flags().StringArrayVar(&projects, "project", nil, "project id to allow for write operations; repeat for multiple projects")
	cmd.Flags().StringVar(&expiresIn, "expires-in", apikey.DefaultExpiresIn, "expiry: 30d, 90d, 365d, or never")
	return cmd
}

func newKeyListCommand(configPath *string, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List API keys",
		Example: `detent key list`,
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			result, err := runKeyList(cmd.Context(), *configPath, opts)
			if err != nil {
				return cliAPIKeyError(err)
			}
			return out.Write(func(out io.Writer) error {
				for _, key := range result.Keys {
					if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", key.ID, key.Name, key.Key, key.Scope, key.Status); err != nil {
						return err
					}
				}
				return nil
			}, result)
		},
	}
	return cmd
}

func newKeyRotateCommand(configPath *string, opts options) *cobra.Command {
	var grace string
	cmd := &cobra.Command{
		Use:     "rotate KEY_ID",
		Short:   "Rotate an API key",
		Example: `detent key rotate 9d01f066-8680-4d0b-bb82-32f9a37527af --grace 24h`,
		Args: func(cmd *cobra.Command, args []string) error {
			return WrapValidation(cobra.ExactArgs(1)(cmd, args))
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			result, err := runKeyRotate(cmd.Context(), *configPath, opts, args[0], grace)
			if err != nil {
				return cliAPIKeyError(err)
			}
			return out.Write(func(out io.Writer) error {
				_, err := fmt.Fprintf(out, "rotated api key %s\nreplacement: %s\nkey: %s\nscope: %s\nprojects: %s\nexpires: %s\ntoken: %s\n",
					result.OldKeyID,
					result.Key.ID,
					result.Key.Key,
					result.Key.Scope,
					apiKeyProjectText(result.Key.ProjectIDs),
					result.Key.ExpiresAt,
					result.Token,
				)
				return err
			}, result)
		},
	}
	cmd.Flags().StringVar(&grace, "grace", "24h", "old key grace period: 1h, 24h, or 7d")
	return cmd
}

func newKeyRevokeCommand(configPath *string, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "revoke KEY_ID",
		Short:   "Revoke an API key",
		Example: `detent key revoke 9d01f066-8680-4d0b-bb82-32f9a37527af`,
		Args: func(cmd *cobra.Command, args []string) error {
			return WrapValidation(cobra.ExactArgs(1)(cmd, args))
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			result, err := runKeyRevoke(cmd.Context(), *configPath, opts, args[0])
			if err != nil {
				return cliAPIKeyError(err)
			}
			return out.Write(func(out io.Writer) error {
				_, err := fmt.Fprintf(out, "revoked api key %s\n", result.ID)
				return err
			}, result)
		},
	}
	return cmd
}

type keyCreatedResult struct {
	Key   keyResult `json:"key"`
	Token string    `json:"token"`
}

type keyRotatedResult struct {
	OldKeyID string    `json:"old_key_id"`
	Key      keyResult `json:"key"`
	Token    string    `json:"token"`
}

type keyListResult struct {
	Keys []keyResult `json:"keys"`
}

type keyRevokedResult struct {
	ID      string `json:"id"`
	Revoked bool   `json:"revoked"`
}

type keyResult struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Key        string   `json:"key"`
	Scope      string   `json:"scope"`
	Scopes     []string `json:"scopes"`
	ProjectIDs []string `json:"project_ids"`
	Status     string   `json:"status"`
	CreatedAt  string   `json:"created_at"`
	ExpiresAt  string   `json:"expires_at,omitempty"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	RevokedAt  string   `json:"revoked_at,omitempty"`
}

func runKeyAdd(ctx context.Context, configPath string, opts options, req apikey.CreateRequest) (result keyCreatedResult, err error) {
	backend, err := openKeyStore(ctx, configPath, opts)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := backend.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	created, err := apikey.NewService(backend).Create(ctx, req)
	if err != nil {
		return result, err
	}
	return keyCreatedResult{
		Key:   newKeyResult(created.Key, time.Now()),
		Token: created.Token,
	}, nil
}

func runKeyList(ctx context.Context, configPath string, opts options) (result keyListResult, err error) {
	backend, err := openKeyStore(ctx, configPath, opts)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := backend.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	keys, err := backend.ListAPIKeys(ctx)
	if err != nil {
		return result, err
	}
	result = keyListResult{Keys: make([]keyResult, 0, len(keys))}
	now := time.Now()
	for _, key := range keys {
		result.Keys = append(result.Keys, newKeyResult(key, now))
	}
	return result, nil
}

func runKeyRotate(ctx context.Context, configPath string, opts options, id string, grace string) (result keyRotatedResult, err error) {
	backend, err := openKeyStore(ctx, configPath, opts)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := backend.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	created, err := apikey.NewService(backend).Rotate(ctx, id, grace)
	if err != nil {
		return result, err
	}
	return keyRotatedResult{
		OldKeyID: strings.TrimSpace(id),
		Key:      newKeyResult(created.Key, time.Now()),
		Token:    created.Token,
	}, nil
}

func runKeyRevoke(ctx context.Context, configPath string, opts options, id string) (result keyRevokedResult, err error) {
	backend, err := openKeyStore(ctx, configPath, opts)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := backend.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	id = strings.TrimSpace(id)
	if err := apikey.NewService(backend).Revoke(ctx, id); err != nil {
		return result, err
	}
	return keyRevokedResult{ID: id, Revoked: true}, nil
}

// keyStoreBusyTimeout is deliberately much longer than the store default:
// key commands are short-lived one-shot writers that contend with a running
// detent server, so waiting beats failing with SQLITE_BUSY.
const keyStoreBusyTimeout = 30 * time.Second

func openKeyStore(ctx context.Context, configPath string, opts options) (storepkg.Store, error) {
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return nil, err
	}
	cfg, err := opts.read(resolution.Path)
	if err != nil {
		return nil, err
	}
	return storepkg.Open(ctx, storepkg.Config{
		Backend: storepkg.BackendSQLite,
		Path: runtimeStorePath(BootConfig{
			Global: cfg,
		}),
		BusyTimeout: keyStoreBusyTimeout,
	})
}

func newKeyResult(key storepkg.APIKey, now time.Time) keyResult {
	return keyResult{
		ID:         key.ID,
		Name:       key.Name,
		Key:        key.PrefixLast4,
		Scope:      string(apikey.HighestScope(key.Scopes)),
		Scopes:     append([]string(nil), key.Scopes...),
		ProjectIDs: append([]string(nil), key.ProjectIDs...),
		Status:     apiKeyStatus(key, now),
		CreatedAt:  formatKeyTime(&key.CreatedAt),
		ExpiresAt:  formatKeyTime(key.ExpiresAt),
		LastUsedAt: formatKeyTime(key.LastUsedAt),
		RevokedAt:  formatKeyTime(key.RevokedAt),
	}
}

func apiKeyStatus(key storepkg.APIKey, now time.Time) string {
	if key.RevokedAt != nil {
		return "revoked"
	}
	if key.ExpiresAt != nil && !key.ExpiresAt.After(now) {
		return "expired"
	}
	return "active"
}

func apiKeyProjectText(projectIDs []string) string {
	if len(projectIDs) == 0 {
		return "All"
	}
	return strings.Join(projectIDs, ",")
}

func formatKeyTime(value *time.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func cliAPIKeyError(err error) error {
	switch {
	case errors.Is(err, apikey.ErrNameRequired),
		errors.Is(err, apikey.ErrInvalidExpiry),
		errors.Is(err, apikey.ErrInvalidGrace),
		errors.Is(err, apikey.ErrInvalidScope),
		errors.Is(err, apikey.ErrKeyExpired),
		errors.Is(err, apikey.ErrKeyLimitExceeded):
		return WrapValidation(err)
	case storepkg.IsBusy(err):
		return fmt.Errorf("%w: a running detent server is holding the runtime database write lock; retry in a moment", err)
	default:
		return err
	}
}
