package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func captureIncorporationObjects(ctx context.Context, directory, base, head, target string) (pack []byte, err error) {
	changed, err := runGitAt(ctx, directory, "diff", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", "-z", base, head, "--")
	if err != nil {
		return nil, err
	}
	paths := make(map[string]bool)
	for _, path := range strings.Split(changed, "\x00") {
		paths[path] = true
	}
	var objects []string
	for _, commit := range []string{base, head, target} {
		tree, err := runGitAt(ctx, directory, "rev-parse", commit+"^{tree}")
		if err != nil {
			return nil, err
		}
		entries, err := runGitAt(ctx, directory, "ls-tree", "-r", "-t", "-z", "--format=%(objecttype)%x09%(objectname)%x09%(path)", commit)
		if err != nil {
			return nil, err
		}
		objects = append(objects, commit, strings.TrimSpace(tree))
		for line := range strings.SplitSeq(entries, "\x00") {
			fields := strings.SplitN(line, "\t", 3)
			if len(fields) != 3 {
				continue
			}
			if fields[0] == "tree" || fields[0] == "blob" && (paths[fields[2]] || filepath.Base(fields[2]) == ".gitattributes") {
				objects = append(objects, fields[1])
			}
		}
	}
	slices.Sort(objects)
	objects = slices.Compact(objects)
	scratch, err := changeSourceScratch()
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	prefix := filepath.Join(scratch, "incorporation")
	hash, err := runGitAtWithInput(ctx, directory, nil, false, strings.Join(objects, "\n")+"\n", "pack-objects", "--delta-base-offset", prefix)
	if err != nil {
		return nil, err
	}
	path := prefix + "-" + strings.TrimSpace(hash) + ".pack"
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > tracker.MaxChangeSourceBytes {
		return nil, errors.New("incorporation objects exceed source limit")
	}
	return os.ReadFile(path)
}

func VerifyLandingIncorporation(ctx context.Context, proof tracker.LandingIncorporation, pack []byte) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if !validLandingHead(proof.SourceBaseSHA) || !validLandingHead(proof.SourceHeadSHA) || !validLandingHead(proof.TargetHeadSHA) || len(pack) == 0 || len(pack) > tracker.MaxChangeSourceBytes {
		return errors.New("incorporation requires immutable identities and bounded objects")
	}
	if err := changeSourcePackBudget(ctx, pack); err != nil {
		return err
	}
	scratch, err := changeSourceScratch()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	env := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_NO_REPLACE_OBJECTS=1"}
	git := func(input []byte, args ...string) (string, error) {
		command := exec.CommandContext(ctx, "git")
		command.Args = append(command.Args, "-C", scratch)
		command.Args = append(command.Args, args...)
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "GIT_") {
				command.Env = append(command.Env, value)
			}
		}
		command.Env = append(command.Env, env...)
		command.Stdin = bytes.NewReader(input)
		command.WaitDelay = workspaceCommandWaitDelay
		procgroup.Configure(ctx, command)
		output, err := command.Output()
		return string(output), err
	}
	if _, err := git(nil, "init", "--bare", "--template="); err != nil {
		return err
	}
	if _, err := git(pack, "-c", "pack.threads=1", "index-pack", "--stdin", "--fsck-objects"); err != nil {
		return errors.New("invalid incorporation objects")
	}
	tree, err := git(nil, "rev-parse", proof.TargetHeadSHA+"^{tree}")
	if err != nil || strings.TrimSpace(tree) != proof.TargetTreeSHA {
		return errors.New("incorporation target tree differs")
	}
	merged, err := git(nil, "merge-tree", "--write-tree", "--merge-base="+proof.SourceBaseSHA, proof.TargetHeadSHA, proof.SourceHeadSHA)
	if err != nil || strings.TrimSpace(merged) != proof.MergeTreeSHA || proof.MergeTreeSHA != proof.TargetTreeSHA {
		return errors.New("reviewed source is not incorporated into target")
	}
	return nil
}

func (r LandResult) MarshalJSON() ([]byte, error) {
	type landing LandResult
	var objects []byte
	if r.Incorporation != nil {
		objects = r.Incorporation.Objects
	}
	return json.Marshal(struct {
		landing
		Objects []byte `json:"incorporation_objects,omitempty"`
	}{landing: landing(r), Objects: objects})
}

func (r *LandResult) UnmarshalJSON(data []byte) error {
	type landing LandResult
	var decoded struct {
		landing
		Objects []byte `json:"incorporation_objects,omitempty"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = LandResult(decoded.landing)
	if r.Incorporation != nil {
		r.Incorporation.Objects = decoded.Objects
	}
	return nil
}
