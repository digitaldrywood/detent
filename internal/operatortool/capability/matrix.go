package capability

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

//go:generate go run ./viewgen

//go:embed matrix.json
var matrixJSON []byte

type Matrix struct {
	Schema     int         `json:"schema"`
	Parent     string      `json:"parent"`
	Operations []Operation `json:"operations"`
}

type Operation struct {
	ID            string         `json:"id"`
	Summary       string         `json:"summary"`
	Audience      string         `json:"audience"`
	Sources       []Candidate    `json:"sources"`
	Availability  []Availability `json:"availability"`
	Authority     Authority      `json:"authority"`
	Preconditions []string       `json:"preconditions"`
	Confirmation  []Confirmation `json:"confirmation"`
	Application   string         `json:"application"`
	Extraction    string         `json:"extraction"`
	Tool          Tool           `json:"tool"`
	Owner         string         `json:"owner"`
	Status        string         `json:"status"`
	Coverage      string         `json:"coverage"`
	Decision      string         `json:"decision"`
}
type Availability struct {
	Deployment  string   `json:"deployment"`
	Trackers    []string `json:"trackers"`
	Service     string   `json:"service"`
	Unsupported string   `json:"unsupported,omitempty"`
}
type Authority struct {
	Role            string `json:"role"`
	CredentialScope string `json:"credential_scope"`
	ProjectGrant    string `json:"project_grant"`
	Ownership       string `json:"ownership"`
}
type Confirmation struct {
	When  string `json:"when"`
	Class string `json:"class"`
}
type Tool struct {
	Set         string       `json:"set"`
	Name        string       `json:"name"`
	Arguments   string       `json:"arguments"`
	Result      string       `json:"result"`
	Annotations *Annotations `json:"annotations,omitempty"`
}

type Annotations struct {
	ReadOnly    bool `json:"readOnly"`
	Destructive bool `json:"destructive"`
	Idempotent  bool `json:"idempotent"`
	OpenWorld   bool `json:"openWorld"`
}

// Load returns a fresh fixture. It is planning data, never authorization or a live tool catalog.
func Load() (Matrix, error) {
	var matrix Matrix
	decoder := json.NewDecoder(bytes.NewReader(matrixJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&matrix); err != nil {
		return Matrix{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Matrix{}, fmt.Errorf("matrix trailing data: %w", err)
	}
	return matrix, nil
}

// Validate rejects uncovered definitions, stale decisions, duplicate identities/site ownership,
// and operator omissions disguised as exclusions. requireParity is reserved for #3259's final acceptance.
func Validate(matrix Matrix, candidates []Candidate, requireParity bool) error {
	var problems []string
	if matrix.Schema != 1 || matrix.Parent != "digitaldrywood/detent#3259" {
		problems = append(problems, "unsupported matrix schema or parent")
	}
	actual := map[string]bool{}
	details := map[string]Candidate{}
	for _, c := range candidates {
		if actual[c.Key()] {
			problems = append(problems, "duplicate discovered source "+c.Key())
		}
		actual[c.Key()] = true
		details[c.Key()] = c
	}
	seen := map[string]string{}
	ids := map[string]bool{}
	for _, op := range matrix.Operations {
		if op.ID == "" || ids[op.ID] {
			problems = append(problems, "duplicate or empty operation ID "+op.ID)
		}
		ids[op.ID] = true
		if len(op.Sources) == 0 {
			problems = append(problems, "orphan operation "+op.ID)
		}
		for _, site := range op.Sources {
			key := site.Key()
			if previous, ok := seen[key]; ok {
				problems = append(problems, "duplicate source ownership "+op.ID+" / "+previous+": "+key)
			}
			seen[key] = op.ID
			if found, ok := details[key]; ok && (found.Method != site.Method || found.Path != site.Path || found.Handler != site.Handler || found.ResolvedPath != site.ResolvedPath) {
				problems = append(problems, "source metadata mismatch "+op.ID+": "+key)
			}
			if !actual[key] {
				problems = append(problems, "orphan source "+op.ID+": "+key)
			}
		}
		switch op.Audience {
		case "operator", "staff", "worker", "transport", "asset", "authentication", "local_ui":
		default:
			problems = append(problems, "invalid audience "+op.ID)
		}
		if !validOwner(op.Owner) {
			problems = append(problems, "invalid child owner "+op.ID)
		}
		if op.Summary == "" || op.Decision == "" || op.Application == "" || op.Extraction == "" || op.Coverage == "" || len(op.Preconditions) == 0 || len(op.Availability) == 0 || len(op.Confirmation) == 0 || op.Authority.Role == "" || op.Authority.CredentialScope == "" || op.Authority.ProjectGrant == "" || op.Authority.Ownership == "" {
			problems = append(problems, "incomplete decision "+op.ID)
		}
		availability := map[string]bool{}
		for _, a := range op.Availability {
			switch a.Deployment {
			case "self_hosted", "hosted_dedicated", "hosted_shared", "credential_maintenance":
			default:
				problems = append(problems, "invalid deployment "+op.ID)
			}
			for _, tracker := range a.Trackers {
				key := a.Deployment + "/" + tracker
				if availability[key] {
					problems = append(problems, "duplicate availability "+op.ID+": "+key)
				}
				availability[key] = true
				if !oneOf(tracker, "github", "native") {
					problems = append(problems, "unknown tracker "+op.ID)
				}
			}
			if len(a.Trackers) == 0 || a.Service == "" {
				problems = append(problems, "incomplete availability "+op.ID)
			}
		}
		for _, c := range op.Confirmation {
			if c.When == "" || !oneOf(c.Class, "none", "operator", "connection", "not_applicable") {
				problems = append(problems, "invalid confirmation "+op.ID)
			}
		}
		if !oneOf(op.Status, "pending", "implemented", "excluded") {
			problems = append(problems, "invalid status "+op.ID)
		}
		if op.Audience == "operator" {
			if op.Status == "excluded" {
				problems = append(problems, "operator operation excluded "+op.ID)
			}
			if op.Tool.Set == "" || op.Tool.Name == "" || op.Tool.Arguments == "" || op.Tool.Result == "" || op.Tool.Annotations == nil {
				problems = append(problems, "missing typed tool proposal "+op.ID)
			}
			if requireParity && op.Status != "implemented" {
				problems = append(problems, "unimplemented operator operation "+op.ID)
			}
		} else if op.Status == "excluded" && op.Tool.Name != "" {
			problems = append(problems, "excluded site exposes operator tool "+op.ID)
		} else if op.Status == "excluded" && strings.Contains(op.Decision, "future") {
			problems = append(problems, "future-proof exclusion "+op.ID)
		}
	}
	for key := range actual {
		if _, ok := seen[key]; !ok {
			problems = append(problems, "uncovered source "+key)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("capability drift:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}
func oneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func validOwner(s string) bool {
	for n := 3335; n <= 3347; n++ {
		if s == fmt.Sprintf("digitaldrywood/detent#%d", n) {
			return true
		}
	}
	return false
}

// RenderMarkdown derives the readable view from the same fixture, not a second inventory.
func RenderMarkdown(matrix Matrix) string {
	var b strings.Builder
	b.WriteString("# Dashboard capability matrix\n\nGenerated from [matrix.json](../internal/operatortool/capability/matrix.json). See [the inventory contract](mcp-capabilities.md) for updates and final parity validation. Pending rows are parity work, not exclusions.\n\n")
	for _, op := range matrix.Operations {
		toolSet, toolName, arguments, result := op.Tool.Set, op.Tool.Name, op.Tool.Arguments, op.Tool.Result
		if toolName == "" {
			toolSet, toolName, arguments, result = "boundary", "no_tool", "not applicable", "explicit source decision"
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n- Audience: %s; status: **%s**; owner: %s.\n- Decision: %s\n- Tool: `%s.%s` — %s → %s\n- Authority: role %s; credential %s; project %s; ownership %s.\n- Application: %s\n- Extraction: %s\n- Preconditions: %s\n- Coverage: %s\n", op.ID, op.Summary, op.Audience, op.Status, op.Owner, op.Decision, toolSet, toolName, arguments, result, op.Authority.Role, op.Authority.CredentialScope, op.Authority.ProjectGrant, op.Authority.Ownership, op.Application, op.Extraction, strings.Join(op.Preconditions, "; "), op.Coverage)
		if a := op.Tool.Annotations; a != nil {
			fmt.Fprintf(&b, "- Proposed hints: readOnly=%t; destructive=%t; idempotent=%t; openWorld=%t. Authorization/confirmation still apply.\n", a.ReadOnly, a.Destructive, a.Idempotent, a.OpenWorld)
		}
		for _, a := range op.Availability {
			fmt.Fprintf(&b, "- Availability: %s / %s / %s", a.Deployment, strings.Join(a.Trackers, ","), a.Service)
			if a.Unsupported != "" {
				fmt.Fprintf(&b, " — unavailable: %s", a.Unsupported)
			}
			b.WriteByte('\n')
		}
		for _, c := range op.Confirmation {
			fmt.Fprintf(&b, "- Confirmation: %s → %s\n", c.When, c.Class)
		}

		b.WriteString("\nSources: ")
		for i, c := range op.Sources {
			if i > 0 {
				b.WriteString(", ")
			}
			label := fmt.Sprintf("%s:%d", c.Source, c.Line)
			if c.Kind == "route" {
				path := c.ResolvedPath
				if path == "" {
					path = c.Path
				}
				label = c.Method + " " + path
			}
			fmt.Fprintf(&b, "[%s](../%s#L%d)", strings.ReplaceAll(label, "|", "\\|"), c.Source, c.Line)
		}

		b.WriteByte('\n')
	}
	return b.String()
}
