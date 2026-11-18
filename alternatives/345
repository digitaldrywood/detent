package doctor

import (
	"os"
	"runtime"
	"strings"
)

func CommandEnvironment(base []string, overrides []string) []string {
	return commandEnvironmentForOS(base, overrides, runtime.GOOS)
}

func commandEnvironmentForOS(base []string, overrides []string, goos string) []string {
	environment := append([]string(nil), base...)
	for _, override := range overrides {
		name, _, ok := strings.Cut(override, "=")
		if !ok {
			continue
		}
		values := environmentValues(environment)
		if goos == "windows" {
			values = environmentValuesFolded(environment)
		}
		override = name + "=" + os.Expand(strings.TrimPrefix(override, name+"="), func(key string) string {
			if goos == "windows" {
				key = strings.ToUpper(key)
			}
			return values[key]
		})
		filtered := environment[:0]
		for _, entry := range environment {
			candidate, _, candidateOK := strings.Cut(entry, "=")
			matches := candidateOK && candidate == name
			if goos == "windows" {
				matches = candidateOK && strings.EqualFold(candidate, name)
			}
			if !matches {
				filtered = append(filtered, entry)
			}
		}
		environment = append(filtered, override)
	}
	return environment
}
func environmentValuesFolded(environment []string) map[string]string {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[strings.ToUpper(name)] = value
		}
	}
	return values
}
func environmentValues(environment []string) map[string]string {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = value
		}
	}
	return values
}
