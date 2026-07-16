package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/auth"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/store"
)

type authLinkResult struct {
	Email     string `json:"email"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

func newAuthCommand(configPath *string, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "auth",
		Short:   "Manage dashboard authentication",
		Example: "detent auth link operator@example.com",
	}
	cmd.AddCommand(newAuthLinkCommand(configPath, opts))
	return cmd
}

func newAuthLinkCommand(configPath *string, opts options) *cobra.Command {
	return &cobra.Command{
		Use:     "link <email>",
		Short:   "Create a one-time dashboard sign-in link",
		Example: "detent auth link operator@example.com",
		Args: func(cmd *cobra.Command, args []string) error {
			return WrapValidation(cobra.ExactArgs(1)(cmd, args))
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			result, err := runAuthLink(cmd.Context(), *configPath, opts, args[0])
			if err != nil {
				return cliAuthError(err)
			}
			return out.Write(func(writer io.Writer) error {
				_, err := fmt.Fprintln(writer, result.URL)
				return err
			}, result)
		},
	}
}

func runAuthLink(ctx context.Context, configPath string, opts options, email string) (result authLinkResult, err error) {
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return result, err
	}
	cfg, err := opts.read(resolution.Path)
	if err != nil {
		return result, err
	}
	if !cfg.Auth.MagicLinkEnabled() {
		return result, errors.New("magic link authentication is not enabled")
	}
	linkTTL, err := cfg.Auth.LinkTTLDuration()
	if err != nil {
		return result, err
	}
	sessionTTL, err := cfg.Auth.SessionTTLDuration()
	if err != nil {
		return result, err
	}
	backend, err := store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path: runtimeStorePath(BootConfig{
			Global: cfg,
		}),
		BusyTimeout: 30 * time.Second,
	})
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := backend.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	service, err := auth.NewService(auth.Config{
		AllowedEmails: cfg.Auth.AllowedEmails,
		LinkTTL:       linkTTL,
		SessionTTL:    sessionTTL,
		PublicURL:     authPublicURL(cfg),
	}, backend, nil)
	if err != nil {
		return result, err
	}
	link, expiresAt, err := service.CreateLink(ctx, email, "/")
	if err != nil {
		return result, err
	}
	return authLinkResult{
		Email:     strings.ToLower(strings.TrimSpace(email)),
		URL:       link,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	}, nil
}

func authPublicURL(cfg globalconfig.Config) string {
	if publicURL := strings.TrimSpace(cfg.Auth.PublicURL); publicURL != "" {
		return publicURL
	}
	port := defaultWebPort
	if cfg.Port != nil && *cfg.Port > 0 {
		port = *cfg.Port
	}
	return "http://" + net.JoinHostPort(dashboardHost, strconv.Itoa(port))
}

func cliAuthError(err error) error {
	switch {
	case errors.Is(err, auth.ErrEmailNotAllowed):
		return WrapValidation(err)
	case store.IsBusy(err):
		return fmt.Errorf("%w: the running detent server is busy; retry in a moment", err)
	default:
		return err
	}
}
