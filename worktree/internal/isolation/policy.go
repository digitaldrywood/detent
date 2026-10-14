package isolation

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

const (
	Sandbox       = "sandbox"
	NativeTrusted = "native-trusted"
)

var ErrSandboxUnavailable = errors.New("backend sandbox is unavailable on this platform")

func SandboxAvailable() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "linux"
}

type Policy struct {
	ExtraNetworkDomains []string `json:"extra_network_domains,omitempty"`
	AllowLocalBinding   bool     `json:"allow_local_binding,omitempty"`
	Tier                string   `json:"tier"`
	WritableRoots       []string `json:"writable_roots,omitempty"`
	HostServices        []string `json:"host_services,omitempty"`
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
	if err := ValidateExtraNetworkDomains(p.ExtraNetworkDomains); err != nil {
		return err
	}
	if p.Tier != Sandbox && p.Tier != NativeTrusted {
		return errors.New("unsupported isolation tier")
	}
	if p.Tier == NativeTrusted {
		return nil
	}
	if !SandboxAvailable() {
		return ErrSandboxUnavailable
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

// Extra domains are exact DNS hosts, never wildcard, IP, port or URL grants.
func ValidateExtraNetworkDomains(domains []string) error {
	if len(domains) > 64 {
		return errors.New("extra network domains may contain at most 64 exact DNS hosts")
	}
	seen := make(map[string]bool)
	for _, domain := range domains {
		if len(domain) > 253 || domain != strings.ToLower(domain) || net.ParseIP(domain) != nil || !strings.Contains(domain, ".") || seen[domain] {
			return errors.New("extra network domains must be unique lowercase exact DNS hosts")
		}
		for _, label := range strings.Split(domain, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return errors.New("extra network domains must be exact DNS hosts")
			}
			for _, char := range label {
				if char != '-' && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
					return errors.New("extra network domains must be exact DNS hosts")
				}
			}
		}
		seen[domain] = true
	}
	return nil
}

type policyContextKey struct{}

func WithPolicy(ctx context.Context, policy Policy) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	policy.HostServices = slices.Clone(policy.HostServices)
	policy.ExtraNetworkDomains = slices.Clone(policy.ExtraNetworkDomains)
	policy.WritableRoots = slices.Clone(policy.WritableRoots)
	return context.WithValue(ctx, policyContextKey{}, policy)
}

func FromContext(ctx context.Context) (Policy, bool) {
	if ctx == nil {
		return Policy{}, false
	}
	policy, ok := ctx.Value(policyContextKey{}).(Policy)
	policy.HostServices = slices.Clone(policy.HostServices)
	policy.ExtraNetworkDomains = slices.Clone(policy.ExtraNetworkDomains)
	policy.WritableRoots = slices.Clone(policy.WritableRoots)
	return policy, ok
}
