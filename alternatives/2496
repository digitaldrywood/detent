package releaseprovenance

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

type ObservedCheck struct {
	Name          string
	Status        string
	Conclusion    string
	Commit        string
	CheckRunID    int64
	StatusID      int64
	IntegrationID int64
}

type RepositoryRequirement struct {
	Name          string
	IntegrationID int64
}

type GitHubEvidence struct {
	RequiredChecks []RepositoryRequirement
	Checks         []ObservedCheck
}

func VerifyGitHubEvidence(manifest Manifest, evidence GitHubEvidence) error {
	if err := Validate(manifest, manifest.Repository, manifest.Tag, manifest.Commit); err != nil {
		return err
	}
	required, err := normalizedRequirements(evidence.RequiredChecks)
	if err != nil {
		return err
	}
	if len(required) == 0 {
		return errors.New("authenticated GitHub evidence has no mandatory checks")
	}
	requiredByName := make(map[string]RepositoryRequirement, len(required))
	for _, requirement := range required {
		requiredByName[requirement.Name] = requirement
	}
	declaredNames := make([]string, 0, len(manifest.Checks))
	for _, check := range manifest.Checks {
		declaredNames = append(declaredNames, check.Name)
	}
	declaredNames = normalizedNames(declaredNames)
	for _, requirement := range required {
		if !slices.Contains(declaredNames, requirement.Name) {
			return fmt.Errorf("release provenance omits authenticated repository requirement %q", requirement.Name)
		}
	}

	for _, declaredCheck := range manifest.Checks {
		requirement := requiredByName[declaredCheck.Name]
		if requirement.IntegrationID != 0 && declaredCheck.CheckRunID <= 0 {
			return fmt.Errorf("mandatory check %q requires check-run evidence from integration %d", declaredCheck.Name, requirement.IntegrationID)
		}
		found := false
		matchedIntegrationID := int64(0)
		for _, observed := range evidence.Checks {
			if observed.Name != declaredCheck.Name || !strings.EqualFold(strings.TrimSpace(observed.Commit), strings.TrimSpace(manifest.Commit)) {
				continue
			}
			if requirement.IntegrationID != 0 && observed.IntegrationID != requirement.IntegrationID {
				continue
			}
			if declaredCheck.CheckRunID > 0 && observed.CheckRunID != declaredCheck.CheckRunID {
				continue
			}
			if declaredCheck.StatusID > 0 && observed.StatusID != declaredCheck.StatusID {
				continue
			}
			if declaredCheck.CheckRunID > 0 && observed.StatusID != 0 {
				continue
			}
			if declaredCheck.StatusID > 0 && (observed.CheckRunID != 0 || observed.IntegrationID != 0) {
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(observed.Status), "completed") || !strings.EqualFold(strings.TrimSpace(observed.Conclusion), "success") {
				continue
			}
			found = true
			matchedIntegrationID = observed.IntegrationID
			break
		}
		if !found {
			return fmt.Errorf("mandatory check %q has no authenticated completed/success evidence for commit %s", declaredCheck.Name, manifest.Commit)
		}
		for _, observed := range evidence.Checks {
			if observed.Name != declaredCheck.Name || observed.IntegrationID != matchedIntegrationID {
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(observed.Commit), strings.TrimSpace(manifest.Commit)) {
				continue
			}
			switch {
			case declaredCheck.CheckRunID > 0 && observed.CheckRunID > declaredCheck.CheckRunID:
				return fmt.Errorf("mandatory check %q has newer authenticated check run %d than declared successful run %d", declaredCheck.Name, observed.CheckRunID, declaredCheck.CheckRunID)
			case declaredCheck.StatusID > 0 && observed.StatusID > declaredCheck.StatusID:
				return fmt.Errorf("mandatory check %q has newer authenticated commit status %d than declared successful status %d", declaredCheck.Name, observed.StatusID, declaredCheck.StatusID)
			}
		}
	}
	return nil
}

func normalizedRequirements(values []RepositoryRequirement) ([]RepositoryRequirement, error) {
	byName := make(map[string]RepositoryRequirement, len(values))
	for _, value := range values {
		value.Name = strings.TrimSpace(value.Name)
		if value.Name == "" {
			continue
		}
		if existing, ok := byName[value.Name]; ok {
			if existing.IntegrationID != 0 && value.IntegrationID != 0 && existing.IntegrationID != value.IntegrationID {
				return nil, fmt.Errorf("authenticated repository requirements disagree on integration for %q", value.Name)
			}
			if existing.IntegrationID != 0 {
				continue
			}
		}
		byName[value.Name] = value
	}
	result := make([]RepositoryRequirement, 0, len(byName))
	for _, value := range byName {
		result = append(result, value)
	}
	slices.SortFunc(result, func(left RepositoryRequirement, right RepositoryRequirement) int {
		return strings.Compare(left.Name, right.Name)
	})
	return result, nil
}

func normalizedNames(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}
