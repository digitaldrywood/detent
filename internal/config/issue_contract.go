package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/policy"
)

type IssueContract struct {
	Sections []string
	Context  string
}

type IssueContractEvaluation struct {
	Missing           []string
	NeedsConfirmation bool
}

func DefaultIssueContract() IssueContract {
	return IssueContract{Sections: []string{"Acceptance criteria", "Must not break", "How we know it worked"}}
}

func ResolvePolicyIssueContract(descriptor policy.Descriptor) (IssueContract, error) {
	if descriptor.Authored != nil && len(descriptor.Authored.Files) > 0 {
		workflow, err := ApplyNativePolicy(Workflow{Config: Default()}, descriptor)
		if err != nil {
			return IssueContract{}, err
		}
		return ResolveIssueContract(workflow.SharedPrompt)
	}
	prompt := ""
	if descriptor.Configuration != nil {
		prompt = descriptor.Configuration.SharedPrompt
	}
	return ResolveIssueContract(prompt)
}

func ResolveIssueContract(prompt string) (IssueContract, error) {
	contract := DefaultIssueContract()
	contract.Context = prompt
	if !admissionHeadingExists(prompt, "Issue Contract") {
		return contract, nil
	}
	_, text, err := resolveAdmissionSection(prompt, "Issue Contract", "issue contract", BacklogAdmissionEffortFileWorkflow)
	if err != nil {
		return IssueContract{}, err
	}
	contract.Sections = nil
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "* ") {
			continue
		}
		section := strings.TrimSpace(line[2:])
		if strings.HasPrefix(section, "**") {
			section, _, _ = strings.Cut(section[2:], "**")
		} else {
			section, _, _ = strings.Cut(section, " — ")
		}
		section = strings.TrimSpace(section)
		key := issueContractSectionKey(section)
		if key == "" || seen[key] {
			return IssueContract{}, fmt.Errorf("issue contract sections must be nonempty and unique: %q", section)
		}
		seen[key] = true
		contract.Sections = append(contract.Sections, section)
	}
	if len(contract.Sections) == 0 {
		return IssueContract{}, errors.New("issue contract must list required sections")
	}
	return contract, nil
}

func IssueContractSectionDigests(body string) map[string]string {
	sections := issueContractSections(body)
	digests := make(map[string]string, len(sections))
	for section, text := range sections {
		sum := sha256.Sum256([]byte(text))
		digests[section] = hex.EncodeToString(sum[:])
	}
	return digests
}

var criterionBullet = regexp.MustCompile(`^([ \t]*)(?:[-*+] |[0-9]+[.)] )`)

func (c IssueContract) AcceptanceCriteria(body string) []string {
	section := "acceptance criteria"
	if len(c.Sections) > 0 {
		section = issueContractSectionKey(c.Sections[0])
		for _, required := range c.Sections {
			if issueContractSectionKey(required) == "acceptance criteria" {
				section = "acceptance criteria"
				break
			}
		}
	}
	text := issueContractSectionBodies(body)[section]
	indent := -1
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		}
		if !inFence {
			if match := criterionBullet.FindStringSubmatch(line); len(match) > 0 && (indent < 0 || len(match[1]) < indent) {
				indent = len(match[1])
			}
		}
	}
	var criteria []string
	var current strings.Builder
	flush := func() {
		if criterion := strings.TrimSpace(current.String()); criterion != "" {
			criteria = append(criteria, criterion)
		}
		current.Reset()
	}
	inFence = false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if indent < 0 && !inFence {
				flush()
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		}
		if match := criterionBullet.FindStringSubmatch(line); !inFence && len(match) > 0 && len(match[1]) == indent {
			flush()
			trimmed = criterionBullet.ReplaceAllString(line, "")
		}
		if current.Len() > 0 {
			current.WriteByte('\n')
		}
		current.WriteString(trimmed)
	}
	flush()
	return criteria
}

var issueContractMetadata = regexp.MustCompile("(?ms)^```(?:detent-agent|detent-origin|detent-status|detent-completion)[ \t]*\n.*?^```[ \t]*\r?$\n?")

func issueContractSections(body string) map[string]string {
	sections := issueContractSectionBodies(body)
	for section, text := range sections {
		sections[section] = strings.TrimSpace(text)
	}
	return sections
}

func issueContractSectionBodies(body string) map[string]string {
	body = issueContractMetadata.ReplaceAllString(body, "")
	headings := markdownHeadings(body)
	sections := map[string]string{}
	for i, heading := range headings {
		end := len(body)
		for _, next := range headings[i+1:] {
			if next.Level <= heading.Level {
				end = next.Start
				break
			}
		}
		text := strings.Trim(body[heading.End:end], "\r\n")
		if strings.TrimSpace(text) != "" {
			key := issueContractSectionKey(heading.Title)
			if _, duplicate := sections[key]; duplicate {
				sections[key] = ""
			} else {
				sections[key] = text
			}
		}
	}
	return sections
}

func issueContractSectionKey(section string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(section), ":"))
}

func (c IssueContract) Evaluate(issue connector.Issue) IssueContractEvaluation {
	if len(c.Sections) == 0 {
		c.Sections = DefaultIssueContract().Sections
	}
	sections := issueContractSections(issue.Description)
	digests := IssueContractSectionDigests(issue.Description)
	_, machineOrigin := issueorigin.Parse(issue.Description)
	machine := machineOrigin || strings.HasSuffix(strings.ToLower(issue.AuthorID), "[bot]")
	result := IssueContractEvaluation{}
	for _, section := range c.Sections {
		key := issueContractSectionKey(section)
		if sections[key] == "" {
			result.Missing = append(result.Missing, section)
			continue
		}
		if issue.IssueContract != nil {
			if issue.IssueContract.ConfirmedSections[key] != digests[key] {
				result.NeedsConfirmation = true
			}
		} else if machine {
			result.NeedsConfirmation = true
		}
	}
	return result
}

func (e IssueContractEvaluation) Satisfied() bool {
	return len(e.Missing) == 0 && !e.NeedsConfirmation
}

var issueContractCommand = regexp.MustCompile("`((?:make|go test|npm run|pnpm|cargo test) [^`\n]+)`")

func (c IssueContract) HumanAction(issue connector.Issue, e IssueContractEvaluation) string {
	var action strings.Builder
	if len(e.Missing) > 0 {
		fmt.Fprintf(&action, "Add or complete these required issue sections: %s. Confirm or edit the draft below before returning this issue to executable work.\n", strings.Join(e.Missing, ", "))
	}
	if e.NeedsConfirmation {
		action.WriteString("A human must confirm the success criteria by editing or saving the issue body.\n")
	}
	sections := issueContractSections(issue.Description)
	goal := firstContractText(sections, "change", "expected behavior", "problem")
	if goal == "" {
		goal = strings.TrimSpace(issue.Title)
	}
	goal = truncateContractDraft(goal)
	for _, section := range e.Missing {
		fmt.Fprintf(&action, "\n## %s\n\n", section)
		switch issueContractSectionKey(section) {
		case "must not break":
			fmt.Fprintf(&action, "- Preserve existing behavior outside the requested change: %s.\n- Confirm which existing behavior and regressions must remain covered.\n", issue.Title)
		case "how we know it worked":
			evidence := firstContractText(sections, "validation", "tests", "evidence")
			if evidence != "" {
				action.WriteString(truncateContractDraft(evidence) + "\n")
			} else if commands := issueContractCommand.FindAllStringSubmatch(c.Context, -1); len(commands) > 0 {
				fmt.Fprintf(&action, "- Run the project validation command `%s`.\n", commands[0][1])
			}
			fmt.Fprintf(&action, "- Observe that %s meets the acceptance criteria; record the result.\n", issue.Title)
		default:
			fmt.Fprintf(&action, "- %s\n- Confirm the observable expected result for %s.\n", goal, issue.Title)
		}
	}
	return strings.TrimSpace(action.String())
}

func firstContractText(sections map[string]string, keys ...string) string {
	for _, key := range keys {
		if text := sections[key]; text != "" {
			return text
		}
	}
	return ""
}

func truncateContractDraft(text string) string {
	runes := []rune(text)
	if len(runes) > 1500 {
		return string(runes[:1500]) + "…"
	}
	return text
}
