package publication

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type Visibility string

const (
	VisibilityUnknown Visibility = "unknown"
	VisibilityPublic  Visibility = "public"
	VisibilityPrivate Visibility = "private"
)

type Source struct {
	Repository string
	Workspaces []string
	Branches   []string
	Logins     []string
}

type Policy struct {
	DestinationRepository          string
	Sources                        []Source
	Visibility                     Visibility
	AllowPublicCrossProjectDetails bool
}

func Protect(text string, policy Policy) string {
	if text == "" || policy.AllowPublicCrossProjectDetails || policy.Visibility == VisibilityPrivate {
		return text
	}
	sources := crossProjectSources(policy.DestinationRepository, policy.Sources)
	if len(sources) == 0 {
		return text
	}

	protected := text
	for index, source := range sources {
		projectToken := "project-" + opaqueOrdinal(index)
		for _, workspace := range source.Workspaces {
			protected = redactWorkspace(protected, workspace)
		}
		branches := append([]string(nil), source.Branches...)
		if generated := generatedBranchPattern(source.Repository); generated != "" {
			branches = append(branches, generated)
		}
		for branchIndex, branch := range normalizedValues(branches) {
			replacement := "branch-" + opaqueOrdinal(branchIndex)
			if strings.HasSuffix(branch, "*") {
				protected = replaceTokenPrefix(protected, strings.TrimSuffix(branch, "*"), replacement)
				continue
			}
			protected = replaceBoundedFold(protected, branch, replacement)
		}
		for loginIndex, login := range normalizedValues(source.Logins) {
			protected = replaceBoundedFold(protected, "@"+strings.TrimPrefix(login, "@"), "@contributor-"+opaqueOrdinal(loginIndex))
			protected = replaceBoundedFold(protected, strings.TrimPrefix(login, "@"), "contributor-"+opaqueOrdinal(loginIndex))
		}
		protected = redactRepositoryReferences(protected, source.Repository, projectToken)
		protected = replaceBoundedFold(protected, source.Repository, projectToken)
		_, repositoryName, _ := strings.Cut(source.Repository, "/")
		_, destinationName, _ := strings.Cut(policy.DestinationRepository, "/")
		if !strings.EqualFold(repositoryName, destinationName) {
			protected = replaceBoundedFold(protected, repositoryName, projectToken)
		}
	}
	return protected
}

func crossProjectSources(destination string, sources []Source) []Source {
	destination = strings.TrimSpace(destination)
	out := make([]Source, 0, len(sources))
	for _, source := range sources {
		source.Repository = strings.TrimSpace(source.Repository)
		if source.Repository == "" || strings.EqualFold(source.Repository, destination) {
			continue
		}
		source.Workspaces = normalizedValues(source.Workspaces)
		source.Branches = normalizedValues(source.Branches)
		source.Logins = normalizedValues(source.Logins)
		out = append(out, source)
	}
	sort.Slice(out, func(i int, j int) bool {
		return strings.ToLower(out[i].Repository) < strings.ToLower(out[j].Repository)
	})
	return out
}

func normalizedValues(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func redactRepositoryReferences(text string, repository string, projectToken string) string {
	pattern := regexp.MustCompile(`(?i)(?:https://github\.com/` + regexp.QuoteMeta(repository) + `/(?:issues|pull)/|` + regexp.QuoteMeta(repository) + `#)([0-9]+)`)
	references := map[string]string{}
	next := 0
	return pattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := pattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		key := strings.TrimLeft(parts[1], "0")
		if key == "" {
			key = "0"
		}
		token, ok := references[key]
		if !ok {
			next++
			token = projectToken + "#" + strconv.Itoa(next)
			references[key] = token
		}
		return token
	})
}

func redactWorkspace(text string, workspace string) string {
	workspace = strings.TrimRight(strings.TrimSpace(workspace), `/\`)
	if workspace == "" {
		return text
	}
	lowerText := strings.ToLower(text)
	lowerWorkspace := strings.ToLower(workspace)
	for offset := 0; ; {
		index := strings.Index(lowerText[offset:], lowerWorkspace)
		if index < 0 {
			return text
		}
		index += offset
		if index > 0 && isIdentifierRune(rune(text[index-1])) {
			offset = index + len(workspace)
			continue
		}
		end := index + len(workspace)
		for end < len(text) && !unicode.IsSpace(rune(text[end])) && !strings.ContainsRune("`\"'<>)]},;", rune(text[end])) {
			end++
		}
		text = text[:index] + "<workspace>" + text[end:]
		lowerText = strings.ToLower(text)
		offset = index + len("<workspace>")
	}
}

func generatedBranchPattern(repository string) string {
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return ""
	}
	replacer := strings.NewReplacer("/", "_", "-", "_", ".", "_")
	return "detent/" + replacer.Replace(repository) + "_*"
}

func replaceTokenPrefix(text string, prefix string, replacement string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return text
	}
	lowerText := strings.ToLower(text)
	lowerPrefix := strings.ToLower(prefix)
	for offset := 0; ; {
		index := strings.Index(lowerText[offset:], lowerPrefix)
		if index < 0 {
			return text
		}
		index += offset
		if index > 0 && isIdentifierRune(rune(text[index-1])) {
			offset = index + len(prefix)
			continue
		}
		end := index + len(prefix)
		for end < len(text) && !unicode.IsSpace(rune(text[end])) && !strings.ContainsRune("`\"'<>)]},;", rune(text[end])) {
			end++
		}
		text = text[:index] + replacement + text[end:]
		lowerText = strings.ToLower(text)
		offset = index + len(replacement)
	}
}

func replaceBoundedFold(text string, value string, replacement string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return text
	}
	lowerText := strings.ToLower(text)
	lowerValue := strings.ToLower(value)
	for offset := 0; ; {
		index := strings.Index(lowerText[offset:], lowerValue)
		if index < 0 {
			return text
		}
		index += offset
		end := index + len(value)
		leftBounded := index == 0 || !isIdentifierRune(rune(text[index-1]))
		rightBounded := end == len(text) || !isIdentifierRune(rune(text[end]))
		if !leftBounded || !rightBounded {
			offset = end
			continue
		}
		text = text[:index] + replacement + text[end:]
		lowerText = strings.ToLower(text)
		offset = index + len(replacement)
	}
}

func isIdentifierRune(value rune) bool {
	return unicode.IsLetter(value) || unicode.IsDigit(value) || value == '_' || value == '-'
}

func opaqueOrdinal(index int) string {
	index++
	var token string
	for index > 0 {
		index--
		token = string(rune('A'+index%26)) + token
		index /= 26
	}
	return token
}
