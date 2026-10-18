package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

// ValidatorTaskContext is shared by the memo identity and the review prompt.
// Evidence is selected by references in the task or canonical Workpad, not by
// comment arrival time. Progress prose and Workpad status are not review inputs.
type ValidatorTaskContext struct {
	Title              string
	Body               string
	Evidence           []string
	MinScore           float64
	BlockOn            []string
	UnverifiedCriteria string
}

var validatorEvidenceURL = regexp.MustCompile(`https?://[^\s<>\x60]+`)

func ValidationContext(issue connector.Issue, policy gate.Config) ValidatorTaskContext {
	cfg := gate.Effective(policy).Validator
	result := ValidatorTaskContext{Title: strings.TrimSpace(issue.Title), Body: normalizeValidatorText(issue.Description), MinScore: cfg.MinScore, BlockOn: slices.Clone(cfg.BlockOn), UnverifiedCriteria: cfg.UnverifiedCriteria}
	slices.Sort(result.BlockOn)
	references := result.Body
	workpad := ""
	for i := len(issue.Comments) - 1; i >= 0; i-- {
		c := issue.Comments[i]
		if strings.Contains(c.Body, "## Codex Workpad") {
			workpad = "\n" + c.Body
			break
		}
	}
	references += workpad
	for _, url := range validatorEvidenceURL.FindAllString(references, -1) {
		url = strings.TrimRight(url, ").,;]")
		evidence := url
		// Inline referenced issue comments so edits to an authoritative decision
		// cannot hide behind an unchanged URL. Other references are immutable
		// evidence identities to inspect during normal review, never approvals.
		comment := ""
		for _, c := range issue.Comments {
			if c.URL == url {
				comment = "\n" + normalizeValidatorText(c.Body)
				break
			}
		}
		evidence += comment
		result.Evidence = append(result.Evidence, evidence)
	}
	slices.Sort(result.Evidence)
	result.Evidence = slices.Compact(result.Evidence)
	return result
}

func ValidationContextDigest(issue connector.Issue, policy gate.Config) string {
	data, err := json.Marshal(ValidationContext(issue, policy))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(append([]byte("detent-validator-context-v1\n"), data...))
	return hex.EncodeToString(sum[:])
}

func normalizeValidatorText(value string) string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
