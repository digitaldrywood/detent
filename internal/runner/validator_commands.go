package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type validatorCommands struct {
	mu      sync.Mutex
	results []gate.CommandResult
	failure error
	run     workspace.ReviewCommandRunner
	info    workspace.Info
	issue   workspace.Issue
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
	provider, ok := v.run.(interface {
		ReviewCommandEnvironment(context.Context, workspace.Info, workspace.Issue, string) gate.CheckEnvironment
	})
	if !ok || receipt.ReceiptID == "" || receipt.Evidence == nil {
		return false
	}
	environment := provider.ReviewCommandEnvironment(ctx, v.info, v.issue, receipt.Command)
	return environment.Known() && receipt.Evidence.Environment == environment
}

func (v *validatorCommands) execute(ctx context.Context, command string) (receipt gate.CommandResult, resultErr error) {
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
	return result, nil
}

func (v *validatorCommands) tools() ([]AgentTool, AgentToolHandler) {
	return []AgentTool{{Name: "detent_run_validation", Description: "Run one acceptance validation command through the host on the immutable reviewed head; returns observed exit status and output. Source changes invalidate the result.", InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],"additionalProperties":false}`)}}, func(ctx context.Context, call AgentToolCall) (AgentToolResult, error) {
		if call.Name != "detent_run_validation" {
			return AgentToolResult{Content: "unsupported tool"}, nil
		}
		var input struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(call.Arguments, &input); err != nil {
			return AgentToolResult{}, err
		}
		result, err := v.execute(ctx, input.Command)
		if err != nil {
			return AgentToolResult{}, err
		}
		data, err := json.Marshal(result)
		return AgentToolResult{Success: result.ExitCode == 0, Content: string(data)}, err
	}
}

func (v *validatorCommands) evidence() ([]gate.CommandResult, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.results), v.failure
}
