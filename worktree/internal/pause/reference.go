package pause

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type Tracker struct {
	ProjectID  string
	Kind       string
	Repository string
}

type ReferenceResolution struct {
	ProjectID  string
	Reference  string
	Repository string
}

func ResolveReference(pausedProjectID string, reference string, trackers []Tracker) (ReferenceResolution, error) {
	pausedProjectID = strings.TrimSpace(pausedProjectID)
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return ReferenceResolution{}, errors.New("pause exit issue reference must not be blank")
	}

	current, ok := trackerByProjectID(trackers, pausedProjectID)
	if !ok {
		return ReferenceResolution{}, fmt.Errorf("paused project %s tracker is unavailable", pausedProjectID)
	}
	if strings.HasPrefix(reference, "#") {
		return resolvedReference(current, reference), nil
	}

	repository, githubReference := githubIssueReferenceRepository(reference)
	if !githubReference {
		if githubTracker(current.Kind) {
			return ReferenceResolution{}, fmt.Errorf("project %s tracker kind %s cannot resolve non-GitHub pause exit issue %s", pausedProjectID, current.Kind, reference)
		}
		return resolvedReference(current, reference), nil
	}

	matches := matchingGitHubTrackers(trackers, repository)
	switch len(matches) {
	case 0:
		if strings.TrimSpace(current.Kind) == "" || strings.EqualFold(strings.TrimSpace(current.Kind), "memory") {
			return resolvedReference(current, reference), nil
		}
		return ReferenceResolution{}, fmt.Errorf("no configured project tracker can resolve GitHub pause exit issue %s", reference)
	case 1:
		return resolvedReference(matches[0], reference), nil
	default:
		for _, match := range matches {
			if strings.EqualFold(strings.TrimSpace(match.ProjectID), pausedProjectID) {
				return resolvedReference(match, reference), nil
			}
		}
		return ReferenceResolution{}, fmt.Errorf("multiple configured project trackers can resolve pause exit issue %s", reference)
	}
}

func trackerByProjectID(trackers []Tracker, projectID string) (Tracker, bool) {
	for _, tracker := range trackers {
		if strings.EqualFold(strings.TrimSpace(tracker.ProjectID), projectID) {
			return tracker, true
		}
	}
	return Tracker{}, false
}

func matchingGitHubTrackers(trackers []Tracker, repository string) []Tracker {
	repository = strings.TrimSpace(repository)
	fullReference := strings.Contains(repository, "/")
	matches := make([]Tracker, 0, 1)
	for _, tracker := range trackers {
		kind := strings.TrimSpace(tracker.Kind)
		if kind != "" && !githubTracker(kind) && !strings.EqualFold(kind, "memory") {
			continue
		}
		trackerRepository := strings.TrimSpace(tracker.Repository)
		if trackerRepository == "" {
			continue
		}
		if fullReference && strings.EqualFold(trackerRepository, repository) {
			matches = append(matches, tracker)
			continue
		}
		if !fullReference && strings.EqualFold(repositoryName(trackerRepository), repository) {
			matches = append(matches, tracker)
		}
	}
	return matches
}

func githubIssueReferenceRepository(reference string) (string, bool) {
	index := strings.LastIndex(reference, "#")
	if index <= 0 || index == len(reference)-1 {
		return "", false
	}
	number, err := strconv.Atoi(strings.TrimSpace(reference[index+1:]))
	if err != nil || number <= 0 {
		return "", false
	}
	repository := strings.TrimSpace(reference[:index])
	return repository, repository != ""
}

func githubTracker(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "github", "github_local":
		return true
	default:
		return false
	}
}

func resolvedReference(tracker Tracker, reference string) ReferenceResolution {
	return ReferenceResolution{
		ProjectID:  strings.TrimSpace(tracker.ProjectID),
		Reference:  trackerIssueReference(reference, tracker.Repository),
		Repository: strings.TrimSpace(tracker.Repository),
	}
}

func repositoryName(repository string) string {
	repository = strings.TrimSpace(repository)
	index := strings.LastIndex(repository, "/")
	if index < 0 {
		return repository
	}
	return strings.TrimSpace(repository[index+1:])
}
