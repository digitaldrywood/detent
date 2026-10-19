package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/releasenotes"
)

type revision struct {
	build releasenotes.Build
	ref   string
}

func git(args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git")
	cmd.Args = append(cmd.Args, args...)
	raw, err := cmd.Output()
	if err != nil {
		return "", errors.New("read release history")
	}
	return string(raw), nil
}

func generate(version, commit string, at time.Time) ([]releasenotes.Build, error) {
	if !releasenotes.IsVersion(version) {
		return []releasenotes.Build{}, nil
	}
	shallow, err := git("rev-parse", "--is-shallow-repository")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(shallow) != "false" {
		return nil, errors.New("release notes require full Git history")
	}
	tags, err := git("for-each-ref", "--format=%(refname:short)%09%(creatordate:iso-strict)", "refs/tags")
	if err != nil {
		return nil, err
	}
	revisions := []revision{}
	for line := range strings.SplitSeq(strings.TrimSpace(tags), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 2 || !releasenotes.IsVersion(fields[0]) || releasenotes.Version(fields[0]) == releasenotes.Version(version) {
			continue
		}
		tagged, err := time.Parse(time.RFC3339, fields[1])
		if err != nil {
			return nil, errors.New("parse release time")
		}
		if tagged.After(at) {
			continue
		}
		revisions = append(revisions, revision{releasenotes.Build{Version: fields[0], BuildTime: tagged}, "refs/tags/" + fields[0]})
	}
	slices.SortFunc(revisions, func(a, b revision) int {
		if order := a.build.BuildTime.Compare(b.build.BuildTime); order != 0 {
			return order
		}
		return strings.Compare(a.build.Version, b.build.Version)
	})
	revisions = append(revisions, revision{releasenotes.Build{Version: version, BuildTime: at}, commit})
	builds := make([]releasenotes.Build, 0, len(revisions))
	seen := map[string]bool{}
	previous := ""
	for _, rev := range revisions {
		args := []string{"log", "--first-parent", "--reverse", "--format=%H%x00%s%x00%b%x00", rev.ref}
		if previous != "" {
			args = append(args, "^"+previous)
		}
		raw, err := git(args...)
		if err != nil {
			return nil, err
		}
		fields := strings.Split(raw, "\x00")
		commits := []releasenotes.Commit{}
		for i := 0; i+2 < len(fields); i += 3 {
			commits = append(commits, releasenotes.Commit{SHA: strings.TrimSpace(fields[i]), Subject: fields[i+1], Body: fields[i+2]})
		}
		builds = append(builds, releasenotes.Append(rev.build, commits, seen))
		previous = rev.ref
	}
	slices.Reverse(builds)
	return builds, nil
}

func run() error {
	version := flag.String("version", "dev", "build version")
	commit := flag.String("commit", "HEAD", "build commit")
	date := flag.String("date", "", "build time")
	output := flag.String("output", "static/release-notes.json", "output path")
	flag.Parse()
	at, err := time.Parse(time.RFC3339, *date)
	if err != nil {
		return errors.New("invalid build time")
	}
	builds, err := generate(*version, *commit, at)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(builds)
	if err != nil {
		return errors.New("encode release notes")
	}
	if err := os.MkdirAll(filepath.Dir(*output), 0755); err != nil {
		return errors.New("create release notes directory")
	}
	if err := os.WriteFile(*output, raw, 0644); err != nil {
		return errors.New("write release notes")
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
