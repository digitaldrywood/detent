package config

import (
	"fmt"
	"strings"
)

func (c IssueContract) ReleaseDeliveryBody() string {
	_, template, err := resolveAdmissionSection(c.Context, "Release delivery contract", "release delivery", BacklogAdmissionEffortFileWorkflow)
	if err != nil {
		return ""
	}
	preamble, _, found := strings.Cut(template, "### ")
	if !found || strings.TrimSpace(preamble) != "Verified release picks of reviewed develop fixes may receive the approved delivery contract." {
		return ""
	}
	names := c.Sections
	if len(names) == 0 {
		names = DefaultIssueContract().Sections
	}
	sections := issueContractSections(template)
	if len(sections) != len(names) || len(markdownHeadings(template)) != len(names) {
		return ""
	}
	var body strings.Builder
	for _, name := range names {
		value := sections[issueContractSectionKey(name)]
		if value == "" {
			return ""
		}
		fmt.Fprintf(&body, "## %s\n%s\n\n", name, value)
	}
	return strings.TrimSpace(body.String())
}
