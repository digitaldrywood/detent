package releasenotes

import (
	"encoding/json"
	"errors"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Entry struct {
	Category string `json:"category"`
	Text     string `json:"text"`
	PRNumber int    `json:"pr_number"`
	PRURL    string `json:"pr_url"`
}

type Build struct {
	Version       string    `json:"version"`
	BuildTime     time.Time `json:"build_time"`
	Patch         bool      `json:"patch"`
	Entries       []Entry   `json:"entries"`
	InternalCount int       `json:"internal_count"`
}

type Commit struct {
	SHA     string
	Subject string
	Body    string
}

var conventional = regexp.MustCompile(`^([a-z]+)(\([^)\r\n]*\))?!?:\s*`)
var pullSuffix = regexp.MustCompile(`(?: \(#[0-9]+\))+$`)
var pullNumber = regexp.MustCompile(`#([0-9]+)`)
var completion = regexp.MustCompile(`^fix: complete prj_[A-Za-z0-9_]+#[0-9]+$`)
var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-op\.(?:g?[a-f0-9]{12}|r\.[1-9][0-9]*\.g[a-f0-9]{12}))?$`)

func Version(value string) string { return "v" + strings.TrimPrefix(value, "v") }

func IsVersion(value string) bool { return versionPattern.MatchString(Version(value)) }

func Read(files fs.FS) ([]Build, error) {
	raw, err := fs.ReadFile(files, "release-notes.json")
	if errors.Is(err, fs.ErrNotExist) {
		return []Build{}, nil
	}
	if err != nil {
		return nil, errors.New("read embedded release notes")
	}
	var builds []Build
	if json.Unmarshal(raw, &builds) != nil {
		return nil, errors.New("decode embedded release notes")
	}
	return builds, nil
}

func Derive(commit Commit) (Entry, bool) {
	suffix := pullSuffix.FindString(commit.Subject)
	numbers := pullNumber.FindStringSubmatch(suffix)
	if len(numbers) != 2 {
		return Entry{}, false
	}
	number, err := strconv.Atoi(numbers[1])
	if err != nil || number <= 0 {
		return Entry{}, false
	}
	subject := strings.TrimSpace(strings.TrimSuffix(commit.Subject, suffix))
	fallback := completion.MatchString(subject)
	if fallback {
		subject = "fix: maintenance update"
		for line := range strings.SplitSeq(commit.Body, "\n") {
			if strings.TrimSpace(line) != "" {
				candidate := strings.TrimSpace(pullSuffix.ReplaceAllString(strings.TrimSpace(line), ""))
				if candidate != "" && !completion.MatchString(candidate) {
					subject = candidate
				}
				break
			}
		}
	}
	kind := ""
	text := subject
	prefix := conventional.FindStringSubmatch(subject)
	if len(prefix) > 0 {
		kind = prefix[1]
		text = strings.TrimPrefix(subject, prefix[0])
	} else if fallback {
		kind = "fix"
	}
	category := map[string]string{"feat": "New", "perf": "Improved", "fix": "Fixed", "revert": "Removed"}[kind]
	text = strings.TrimSpace(pullSuffix.ReplaceAllString(text, ""))
	if fallback && text == "" {
		text = "Maintenance update"
	}
	if category == "" || text == "" {
		return Entry{PRNumber: number}, false
	}
	first, size := utf8.DecodeRuneInString(text)
	text = string(unicode.ToUpper(first)) + text[size:]
	return Entry{Category: category, Text: text, PRNumber: number, PRURL: "https://github.com/digitaldrywood/detent/pull/" + strconv.Itoa(number)}, true
}

func Append(build Build, commits []Commit, seen map[string]bool) Build {
	build.Version = Version(build.Version)
	build.Patch = strings.Contains(build.Version, "-op.")
	build.Entries = []Entry{}
	for _, commit := range commits {
		entry, public := Derive(commit)
		key := commit.SHA
		if entry.PRNumber > 0 {
			key = "pr:" + strconv.Itoa(entry.PRNumber)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if public {
			build.Entries = append(build.Entries, entry)
		} else {
			build.InternalCount++
		}
	}
	return build
}
