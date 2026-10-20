package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type validatorCommands struct {
	mu               sync.Mutex
	results          []gate.CommandResult
	failure          error
	failedValidation map[string]bool
	run              workspace.ReviewCommandRunner
	info             workspace.Info
	issue            workspace.Issue
	base             string
	shell            string
}

func (v *validatorCommands) reuse(tree string, receipts []gate.CommandResult) {
	if tree == "" {
		return
	}
	latest := make(map[string]gate.CommandResult)
	seen := make(map[string]bool)
	var commands []string
	for _, receipt := range receipts {
		if receipt.HeadSHA != v.issue.PullRequestHeadSHA || receipt.TreeSHA != tree {
			continue
		}
		if receipt.ReceiptID != "" {
			if seen[receipt.ReceiptID] {
				continue
			}
			seen[receipt.ReceiptID] = true
		}
		_, exists := latest[receipt.Command]
		if !exists {
			commands = append(commands, receipt.Command)
		}
		latest[receipt.Command] = receipt
	}
	for _, command := range commands {
		receipt := latest[command]
		if workspace.ValidationCoversHead(&receipt, command, v.issue.PullRequestHeadSHA) {
			v.results = append(v.results, receipt)
		}
	}
}

func (v *validatorCommands) canReuse(ctx context.Context, receipt gate.CommandResult) bool {
	if receipt.ReceiptID == "" || receipt.Evidence == nil || !receipt.Evidence.Valid() {
		return false
	}
	provider, ok := v.run.(interface {
		ReviewCommandEvidence(context.Context, workspace.Info, workspace.Issue, string) *gate.CommandEvidence
	})
	if !ok {
		return false
	}
	evidence := provider.ReviewCommandEvidence(ctx, v.info, v.issue, receipt.Command)
	return evidence != nil && evidence.Environment.Known() && evidence.Environment == receipt.Evidence.Environment && evidence.InputsSHA != "" && evidence.InputsSHA == receipt.Evidence.InputsSHA
}

func (v *validatorCommands) execute(ctx context.Context, command string, validation bool) (receipt gate.CommandResult, resultErr error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	defer func() {
		if resultErr != nil {
			v.failure = resultErr
		}
	}()
	result, err := v.run.RunReviewCommand(ctx, v.info, v.issue, command)
	if err != nil {
		return result, fmt.Errorf("%w: validation command: %w", ErrValidatorInfrastructure, err)
	}
	if result.Command != command || result.HeadSHA != v.issue.PullRequestHeadSHA || result.TreeSHA == "" {
		return result, fmt.Errorf("%w: validation command evidence differs from reviewed content", ErrValidatorInfrastructure)
	}

	v.results = append(v.results, result)
	if validation {
		key := result.Command + "\x00" + result.TreeSHA
		if result.ExitCode == 0 {
			delete(v.failedValidation, key)
		} else {
			if v.failedValidation == nil {
				v.failedValidation = make(map[string]bool)
			}
			v.failedValidation[key] = true
		}
	}
	return result, nil
}

func (v *validatorCommands) tools() ([]AgentTool, AgentToolHandler) {
	return []AgentTool{
			{Name: "detent_run_validation", Description: "Run one required acceptance validation command through the host on the immutable reviewed head; a failed command prevents a passing review unless a later run of the same command passes on the same head and tree. Use detent_read_source for source inspection.", InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],"additionalProperties":false}`)},
			{Name: "detent_read_source", Description: "Read a file or list files from the pinned review head or base. Returns a host command receipt for source inspection, never test execution. Pages start at byte offset 0 and default to 16384 bytes; length must be 1..16384. Text boundaries align to complete UTF-8 characters; a short length may return one whole character and an offset within a character includes its beginning. Receipts identify the actual byte range, total size, next_offset and eof; request subsequent relevant pages explicitly. A failed read is retained but may be recovered by a successful read; missing criterion evidence remains unverified.", InputSchema: json.RawMessage(`{"type":"object","properties":{"operation":{"type":"string","enum":["read","list"]},"revision":{"type":"string","enum":["head","base"]},"path":{"type":"string"},"offset":{"type":"integer","minimum":0},"length":{"type":"integer","minimum":1,"maximum":16384}},"required":["operation","revision","path"],"additionalProperties":false}`)},
		}, func(ctx context.Context, call AgentToolCall) (AgentToolResult, error) {
			var command string
			switch call.Name {
			case "detent_run_validation":
				var input struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal(call.Arguments, &input); err != nil {
					return AgentToolResult{}, err
				}
				command = input.Command
			case "detent_read_source":
				var err error
				var selection workspace.ReviewOutputRange
				command, selection, err = v.sourceCommand(call.Arguments)
				if err != nil {
					return AgentToolResult{Content: err.Error()}, nil
				}
				ctx = workspace.WithReviewOutputRange(ctx, selection)
			default:
				return AgentToolResult{Content: "unsupported tool"}, nil
			}
			result, err := v.execute(ctx, command, call.Name == "detent_run_validation")
			if err != nil {
				return AgentToolResult{}, err
			}
			data, err := json.Marshal(result)
			return AgentToolResult{Success: result.ExitCode == 0, Content: string(data)}, err
		}
}

func (v *validatorCommands) sourceCommand(raw json.RawMessage) (string, workspace.ReviewOutputRange, error) {
	var input struct {
		Operation string `json:"operation"`
		Revision  string `json:"revision"`
		Path      string `json:"path"`
		Offset    int64  `json:"offset"`
		Length    *int   `json:"length"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return "", workspace.ReviewOutputRange{}, err
	}
	selection := workspace.ReviewOutputRange{Offset: input.Offset, Length: workspace.ReviewSourcePageBytes}
	if input.Length != nil {
		selection.Length = *input.Length
	}
	if selection.Offset < 0 || selection.Length < 1 || selection.Length > workspace.ReviewSourcePageBytes {
		return "", workspace.ReviewOutputRange{}, errors.New("source offset must be nonnegative and length must be 1..16384 bytes")
	}
	if !fs.ValidPath(input.Path) || strings.ContainsAny(input.Path, "\x00\\") {
		return "", workspace.ReviewOutputRange{}, errors.New("source path must be a repository-relative file or directory")
	}
	commit := v.issue.PullRequestHeadSHA
	switch input.Revision {
	case "head":
	case "base":
		commit = v.base
	default:
		return "", workspace.ReviewOutputRange{}, errors.New("source revision must be the pinned head or base")
	}
	if !changerequest.ValidHash(commit, 40) && !changerequest.ValidHash(commit, 64) {
		return "", workspace.ReviewOutputRange{}, errors.New("pinned source revision is unavailable")
	}
	args := []string{"--no-pager", "--literal-pathspecs"}
	switch input.Operation {
	case "read":
		args = append(args, "show", "--no-ext-diff", "--no-textconv", commit+":"+input.Path)
	case "list":
		args = append(args, "ls-tree", "-r", "--name-only", commit, "--", input.Path)
	default:
		return "", workspace.ReviewOutputRange{}, errors.New("source operation must be read or list")
	}
	return commandshell.WithArgs("git", v.shell, args), selection, nil
}

func (v *validatorCommands) apply(result *gate.ValidatorResult) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	result.Commands = slices.Clone(v.results)
	if len(v.failedValidation) != 0 && result.Verdict == gate.ValidatorVerdictPass {
		result.Verdict = gate.ValidatorVerdictWait
		result.Summary += "\nHost validation command failed; passing test evidence is required."
	}
	return v.failure
}

func (v *validatorCommands) evidence() ([]gate.CommandResult, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.results), v.failure
}
