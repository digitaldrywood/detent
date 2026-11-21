package gate

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/failureattribution"
)

const TemporaryFailureExitCode = 75

const InstanceAttribution = "instance"

func (result CommandResult) InstanceFailure() bool {
	return result.ExitCode != 0 && result.Attribution == InstanceAttribution
}

func (result ValidatorResult) InstanceFailure() bool {
	if result.Attribution == InstanceAttribution {
		return true
	}
	var instanceFailure bool
	for _, command := range result.Commands {
		if command.ExitCode != 0 && !command.InstanceFailure() {
			return false
		}
		instanceFailure = instanceFailure || command.InstanceFailure()
	}
	for _, finding := range result.Findings {
		if finding.Attribution != InstanceAttribution {
			return false
		}
		instanceFailure = true
	}
	return instanceFailure
}

func CommandObservation(result CommandResult) failureattribution.Observation {
	return failureattribution.Observation{Command: result.Command, HeadSHA: result.HeadSHA, ExitCode: result.ExitCode, Output: result.Output, Complete: !result.OutputTruncated && !result.FinishedAt.IsZero()}
}

func AttributeFindings(base, head string, findings []Finding, commands []CommandResult) string {
	values := make([]string, 0, len(findings))
	for i := range findings {
		finding := &findings[i]
		finding.Attribution, finding.AttributionEvidence = failureattribution.Unknown, nil
		for j := len(commands) - 1; j >= 0; j-- {
			command := commands[j]
			if command.Command != finding.CheckCommand {
				continue
			}
			if command.BaseReproduction != nil {
				finding.Attribution, finding.AttributionEvidence = failureattribution.Classify(base, head, finding.CheckDiagnostic, CommandObservation(command), *command.BaseReproduction)
			}
			break
		}
		values = append(values, finding.Attribution)
	}
	return failureattribution.Aggregate(values)
}

func NormalizeAttribution(result *ValidatorResult) {
	values := make([]string, 0, len(result.Findings)+len(result.Commands))
	AttributeFindings(result.BaseSHA, result.HeadSHA, result.Findings, result.Commands)
	for _, finding := range result.Findings {
		values = append(values, finding.Attribution)
	}

	for i := range result.Commands {
		command := &result.Commands[i]
		if command.ExitCode == 0 {
			continue
		}
		NormalizeCommandAttribution(result.BaseSHA, result.HeadSHA, command)
		values = append(values, command.Attribution)
	}
	result.Attribution = failureattribution.Aggregate(values)
}

func NormalizeCommandAttribution(base, head string, result *CommandResult) {
	if result.ExitCode == TemporaryFailureExitCode {
		result.Attribution = InstanceAttribution
		return
	}
	if len(result.Findings) == 0 && result.ExitCode != 0 {
		diagnostic := ""
		if result.BaseReproduction != nil && result.BaseReproduction.Complete && !result.OutputTruncated {
			diagnostic = strings.TrimSpace(result.Output)
		}
		result.Findings = []Finding{{Body: "Command failed", CheckCommand: result.Command, CheckDiagnostic: diagnostic}}
	}
	result.Attribution = AttributeFindings(base, head, result.Findings, []CommandResult{*result})
}
