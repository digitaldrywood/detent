package main

import (
	"fmt"
	"io"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/buildinfo"
)

var version, commit, date = "dev", "none", "unknown"

type versionInfo struct {
	Version   string
	Commit    string
	Date      string
	GoVersion string
	OS        string
	Arch      string
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version metadata",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := io.WriteString(cmd.OutOrStdout(), formatVersionInfo(currentVersionInfo()))
			return err
		},
	}
}

func currentVersionInfo() versionInfo {
	build := currentBuildInfo()
	return versionInfo{
		Version:   build.Version,
		Commit:    build.Commit,
		Date:      build.Date,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
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
