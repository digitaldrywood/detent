package cli

import (
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newAttachCommand(lookup func(string) string, httpClient *http.Client) *cobra.Command {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: time.Minute}
	}
	var target nativeProjectFlags
	var name, contentType string
	cmd := &cobra.Command{Use: "attach <file>", Short: "Upload a Cloud issue attachment and print its markdown reference", Example: "  detent attach screenshot.png --project my-project\n  detent attach trace.txt --project prj_example --organization org_example --hub-url https://cloud.detent.build", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		native, err := target.client(cmd, lookup, httpClient)
		if err != nil {
			return err
		}
		result, err := native.UploadAttachmentFile(cmd.Context(), args[0], name, contentType)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), result.Reference)
		return err
	}}
	target.register(cmd)
	cmd.Flags().StringVar(&name, "name", "", "Attachment filename; defaults to the file's basename")
	cmd.Flags().StringVar(&contentType, "content-type", "", "File content type; inferred when omitted")
	return cmd
}

type nativeProjectFlags struct {
	configPath, project, organization, hubURL, identityFile, tokenEnv string
}

func (f *nativeProjectFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.configPath, "config", "", "Customer global configuration path")
	cmd.Flags().StringVar(&f.project, "project", "", "Configured project name or native project ID")
	cmd.Flags().StringVar(&f.organization, "organization", "", "Native organization ID")
	cmd.Flags().StringVar(&f.hubURL, "hub-url", "", "Explicit Cloud entry URL")
	cmd.Flags().StringVar(&f.identityFile, "identity-file", "", "Enrolled runner identity file")
	cmd.Flags().StringVar(&f.tokenEnv, "token-env", "DETENT_HUB_TOKEN", "Environment variable containing a scoped credential")
}

func (f *nativeProjectFlags) client(cmd *cobra.Command, lookup func(string) string, httpClient *http.Client) (*hubclient.NativeClient, error) {
	cfg := hubclient.Config{URL: f.hubURL, IdentityFile: f.identityFile, HTTPClient: httpClient}
	selectedTokenEnv := f.tokenEnv
	org, id := f.organization, f.project
	if cfg.URL == "" {
		resolution, err := globalconfig.ResolvePath(f.configPath)
		if err != nil {
			return nil, err
		}
		settings, err := globalconfig.Read(resolution.Path)
		if err != nil {
			return nil, err
		}
		cfg.URL, cfg.IdentityFile = settings.Client.URL, settings.Client.IdentityFile
		org = settings.Client.OrganizationID
		if mapped := settings.Client.NativeProjects[f.project]; mapped != "" {
			id = mapped
		}
		if !cmd.Flags().Changed("token-env") && settings.Client.TokenEnvironment != "" {
			selectedTokenEnv = settings.Client.TokenEnvironment
		}
	}
	cfg.TokenSource = func() string { return lookup(selectedTokenEnv) }
	client, err := hubclient.New(cfg)
	if err != nil {
		return nil, err
	}
	return client.Native(tracker.OrganizationID(org), tracker.ProjectID(id))
}
