package isolation

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
)

const (
	Sandbox       = "sandbox"
	NativeTrusted = "native-trusted"
)

type Policy struct {
	Tier          string
	WritableRoots []string
	HostServices  []string
}

type Report map[string][]string

func (r Report) Validate() error {
	if len(r) > 100 {
		return errors.New("too many isolation backends")
	}
	for backend, tiers := range r {
		if backend == "" || len(backend) > 200 || len(tiers) > 2 {
			return errors.New("invalid isolation backend report")
		}
		for i, tier := range tiers {
			if tier != Sandbox && tier != NativeTrusted || slices.Contains(tiers[:i], tier) {
				return errors.New("invalid isolation tier report")
			}
		}
	}
	return nil
}

func (r Report) Supports(tier string) bool {
	if len(r) == 0 || r.Validate() != nil {
		return false
	}
	for _, tiers := range r {
		if !slices.Contains(tiers, tier) {
			return false
		}
	}
	return true
}

func (p Policy) Validate() error {
	if p.Tier != Sandbox && p.Tier != NativeTrusted {
		return errors.New("unsupported isolation tier")
	}
	if p.Tier == NativeTrusted {
		return nil
	}
	if len(p.WritableRoots) == 0 {
		return errors.New("sandbox requires a worktree")
	}
	for _, root := range p.WritableRoots {
		if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
			return errors.New("sandbox roots must be absolute directories below the filesystem root")
		}
	}
	for _, service := range p.HostServices {
		path, ok := strings.CutPrefix(service, "unix:")
		if !ok || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("backend sandbox requires an exact Unix socket path; TCP port grants are unsupported")
		}
	}
	return nil
}

func Domains() []string {
	return []string{"api.openai.com", "chatgpt.com", "api.anthropic.com", "github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com", "proxy.golang.org", "sum.golang.org", "storage.googleapis.com", "registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "crates.io", "index.crates.io", "static.crates.io"}
}
