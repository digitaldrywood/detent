package main

import (
	"fmt"
	"io"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/cli"
	"github.com/digitaldrywood/detent/internal/cloudentry"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

var version, commit, date = "dev", "none", "unknown"

type versionInfo struct {
	HubSchemaVersion          int64  `json:"hub_schema_version"`
	HubSchemaFingerprint      string `json:"hub_schema_fingerprint"`
	RegistrySchemaVersion     int64  `json:"registry_schema_version"`
	RegistrySchemaFingerprint string `json:"registry_schema_fingerprint"`
	AuthSchemaVersion         int64  `json:"auth_schema_version"`
	AuthSchemaFingerprint     string `json:"auth_schema_fingerprint"`
	PlatformSchemaVersion     int64  `json:"platform_schema_version"`
	PlatformSchemaFingerprint string `json:"platform_schema_fingerprint"`
	Version                   string `json:"version"`
	Commit                    string `json:"commit"`
	Date                      string `json:"build_date"`
	GoVersion                 string `json:"go_version"`
	OS                        string `json:"os"`
	Arch                      string `json:"arch"`
	SocketActivationSupported bool   `json:"socket_activation_supported"`
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "Print version metadata",
		Example: "detent version",
		Args:    cli.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := currentVersionInfo()
			var err error
			info.HubSchemaVersion, err = hubserver.LatestSchemaVersion()
			if err != nil {
				return err
			}
			info.HubSchemaFingerprint, err = hubserver.LatestSchemaFingerprint()
			if err != nil {
				return err
			}
			entrySchemas, err := cloudentry.LatestSchemaVersions()
			if err != nil {
				return err
			}
			info.RegistrySchemaVersion, info.AuthSchemaVersion = entrySchemas.Registry, entrySchemas.Auth
			info.RegistrySchemaFingerprint, info.AuthSchemaFingerprint = entrySchemas.RegistryFingerprint, entrySchemas.AuthFingerprint
			info.PlatformSchemaVersion, err = postgres.LatestSchemaVersion()
			if err != nil {
				return err
			}
			info.PlatformSchemaFingerprint, err = postgres.LatestSchemaFingerprint()
			if err != nil {
				return err
			}
			out, err := cli.OutputForCommand(cmd)
			if err != nil {
				return err
			}
			return out.Write(func(out io.Writer) error {
				_, err := io.WriteString(out, formatVersionInfo(info))
				return err
			}, info)
		},
	}
}

func currentVersionInfo() versionInfo {
	build := currentBuildInfo()
	return versionInfo{
		Version:                   build.Version,
		Commit:                    build.Commit,
		Date:                      build.Date,
		GoVersion:                 runtime.Version(),
		OS:                        runtime.GOOS,
		Arch:                      runtime.GOARCH,
		SocketActivationSupported: true,
	}
}

func currentBuildInfo() buildinfo.Info {
	return buildinfo.Resolve(version, commit, date)
}

func formatVersionInfo(info versionInfo) string {
	return fmt.Sprintf(
		"version: %s\ncommit: %s\nbuild date: %s\ngo version: %s\nos/arch: %s/%s\n",
		info.Version,
		info.Commit,
		info.Date,
		info.GoVersion,
		info.OS,
		info.Arch,
	)
}
