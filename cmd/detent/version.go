package main

import (
	"fmt"
	"io"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/cli"
)

var version, commit, date = "dev", "none", "unknown"

type versionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"build_date"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version metadata",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := currentVersionInfo()
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
