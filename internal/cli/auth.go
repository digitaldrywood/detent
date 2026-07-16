package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
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

func newAuthCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "auth",
		Short:   "Manage dashboard authentication",
		Example: "detent auth link operator@example.com\n  detent auth token enable --base-url https://detent.example.com",
	}
	cmd.AddCommand(
		newAuthLinkCommand(configPath, opts),
		newAuthTokenCommand(configPath, host, port, opts),
	)
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

type dashboardTokenResult struct {
	URL string `json:"url"`
}

func newAuthTokenCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "token",
		Short:   "Manage private dashboard URL access",
		Example: "detent auth token rotate --base-url https://detent.example.com",
	}
	cmd.AddCommand(
		newAuthTokenEnableCommand(configPath, host, port, opts),
		newAuthTokenRotateCommand(configPath, host, port, opts),
	)
	return cmd
}

func newAuthTokenEnableCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	var allowWrite bool
	var baseURL string
	cmd := &cobra.Command{
		Use:     "enable",
		Short:   "Enable private dashboard URL access",
		Example: "detent auth token enable --base-url https://detent.example.com",
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := updateDashboardToken(*configPath, derefString(host), derefInt(port, -1), flagChanged(cmd, "port"), baseURL, allowWrite, false, opts)
			if err != nil {
				return err
			}
			return writeDashboardTokenResult(cmd, result)
		},
	}
	cmd.Flags().BoolVar(&allowWrite, "allow-write", false, "allow dashboard mutations for anyone with the private URL")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "public dashboard base URL used in command output")
	return cmd
}

func newAuthTokenRotateCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	var baseURL string
	cmd := &cobra.Command{
		Use:     "rotate",
		Short:   "Rotate the private dashboard URL token",
		Example: "detent auth token rotate --base-url https://detent.example.com",
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := updateDashboardToken(*configPath, derefString(host), derefInt(port, -1), flagChanged(cmd, "port"), baseURL, false, true, opts)
			if err != nil {
				return err
			}
			return writeDashboardTokenResult(cmd, result)
		},
	}
	cmd.Flags().StringVar(&baseURL, "base-url", "", "public dashboard base URL used in command output")
	return cmd
}

func writeDashboardTokenResult(cmd *cobra.Command, result dashboardTokenResult) error {
	out, err := OutputForCommand(cmd)
	if err != nil {
		return err
	}
	return out.Write(func(writer io.Writer) error {
		_, err := fmt.Fprintln(writer, result.URL)
		return err
	}, result)
}

func updateDashboardToken(configPath string, host string, port int, portSet bool, baseURL string, allowWrite bool, rotate bool, opts options) (dashboardTokenResult, error) {
	path, err := resolveConfigPath(configPath, opts)
	if err != nil {
		return dashboardTokenResult{}, err
	}
	cfg, err := opts.read(path)
	if err != nil {
		return dashboardTokenResult{}, err
	}

	mode := strings.ToLower(strings.TrimSpace(cfg.DashboardAccess.Mode))
	if rotate {
		if mode != globalconfig.DashboardAccessModePrivateToken {
			return dashboardTokenResult{}, WrapValidation(errors.New("private dashboard URL access is not enabled"))
		}
		allowWrite = cfg.DashboardAccess.AllowWrite
	} else if mode != "" {
		return dashboardTokenResult{}, WrapValidation(errors.New("private dashboard URL access is already enabled; use detent auth token rotate"))
	}

	token, err := generateDashboardToken(rand.Reader)
	if err != nil {
		return dashboardTokenResult{}, err
	}
	shareURL, err := privateDashboardURL(cfg, host, port, portSet, baseURL, token)
	if err != nil {
		return dashboardTokenResult{}, WrapValidation(err)
	}
	cfg.DashboardAccess = globalconfig.DashboardAccess{
		Mode:       globalconfig.DashboardAccessModePrivateToken,
		Token:      token,
		AllowWrite: allowWrite,
	}
	if err := opts.write(path, cfg); err != nil {
		return dashboardTokenResult{}, err
	}
	return dashboardTokenResult{URL: shareURL}, nil
}

func generateDashboardToken(reader io.Reader) (string, error) {
	if reader == nil {
		return "", errors.New("dashboard token entropy source is required")
	}
	raw := make([]byte, 32)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", fmt.Errorf("generate dashboard token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func privateDashboardURL(cfg globalconfig.Config, host string, port int, portSet bool, baseURL string, token string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		host = strings.Trim(strings.TrimSpace(host), "[]")
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "localhost"
		}
		if !portSet {
			port = defaultWebPort
			if cfg.Port != nil {
				port = *cfg.Port
			}
		}
		baseURL = "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse --base-url: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("--base-url must be an absolute http or https URL without user information")
	}
	if parsed.Scheme == "http" && !privateDashboardLoopbackHost(parsed.Hostname()) {
		return "", errors.New("--base-url must use https unless it targets a loopback host")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("--base-url must not contain a query or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", errors.New("--base-url must not contain a path")
	}
	parsed.Path = "/"
	query := parsed.Query()
	query.Set("token", token)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func privateDashboardLoopbackHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
