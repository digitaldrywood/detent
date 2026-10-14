package statuspage

import (
	"net/url"
	"strings"
)

type Source struct {
	ProjectID          string
	Connector          string
	Provider           string
	BaseURL            string
	RelevantComponents []string
}

func SourceForTracker(projectID string, connector string, baseURL string) Source {
	connector = strings.ToLower(strings.TrimSpace(connector))
	source := Source{
		ProjectID: strings.TrimSpace(projectID),
		Connector: connector,
		BaseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
	}
	switch connector {
	case "github", "github_local":
		source.Provider = "GitHub"
		source.RelevantComponents = []string{"API Requests", "Issues", "Pull Requests", "Git Operations", "Webhooks", "Actions"}
	case "linear":
		source.Provider = "Linear"
		source.RelevantComponents = []string{"EU Region – Linear API", "US Region – Linear API"}
	default:
		source.Provider = strings.TrimSpace(connector)
	}
	return source
}

func (s Source) normalize() (Source, bool) {
	s.ProjectID = strings.TrimSpace(s.ProjectID)
	s.Connector = strings.ToLower(strings.TrimSpace(s.Connector))
	s.Provider = strings.TrimSpace(s.Provider)
	s.BaseURL = strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	parsed, err := url.Parse(s.BaseURL)
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return Source{}, false
	}
	components := make([]string, 0, len(s.RelevantComponents))
	seen := map[string]struct{}{}
	for _, component := range s.RelevantComponents {
		component = strings.TrimSpace(component)
		key := strings.ToLower(component)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		components = append(components, component)
	}
	s.RelevantComponents = components
	return s, true
}
