package config

import (
	"errors"
	"fmt"
	"strings"
)

type ReporterContracts map[string][]string

func DefaultReporterContracts() ReporterContracts {
	return ReporterContracts{
		"bug":            {"Steps to reproduce", "Expected", "Actual", "Where", "Environment"},
		"change_request": {"Current behavior", "Desired behavior", "Why"},
	}
}

func ResolveReporterContracts(prompt string) (ReporterContracts, error) {
	contracts := DefaultReporterContracts()
	if !admissionHeadingExists(prompt, "Reporter Contract") {
		return contracts, nil
	}
	heading, text, err := resolveAdmissionSection(prompt, "Reporter Contract", "reporter contract", BacklogAdmissionEffortFileWorkflow)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, child := range markdownHeadings(text) {
		if child.Level != heading.Level+1 {
			continue
		}
		kind := strings.ReplaceAll(issueContractSectionKey(child.Title), " ", "_")
		if _, ok := contracts[kind]; !ok || seen[kind] {
			return nil, fmt.Errorf("reporter contract type must be bug or change_request and unique: %q", child.Title)
		}
		_, required, err := resolveAdmissionSection(text, child.Title, "reporter contract", BacklogAdmissionEffortFileWorkflow)
		if err != nil {
			return nil, err
		}
		contracts[kind], err = issueContractRequiredSections(required, "reporter contract")
		if err != nil {
			return nil, err
		}
		seen[kind] = true
	}
	if len(seen) == 0 {
		return nil, errors.New("reporter contract must define bug or change_request sections")
	}
	return contracts, nil
}
