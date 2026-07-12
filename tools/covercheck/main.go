package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

type packageStats struct {
	covered int
	total   int
}

type profileStats struct {
	packages map[string]packageStats
	files    map[string]packageStats
}

func (s packageStats) percent() float64 {
	if s.total == 0 {
		return 100
	}

	return float64(s.covered) * 100 / float64(s.total)
}

type coverageFailure struct {
	Package  string
	Coverage float64
	Floor    float64
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("covercheck", flag.ContinueOnError)
	flags.SetOutput(stderr)

	profilePath := flags.String("profile", "", "Go coverprofile path")
	defaultFloor := flags.Float64("floor", 50, "default per-package coverage floor")
	exceptionsPath := flags.String("exceptions", "", "coverage exceptions file")

	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *profilePath == "" {
		fmt.Fprintln(stderr, "-profile is required")
		return 2
	}
	if err := validateFloor(*defaultFloor); err != nil {
		fmt.Fprintf(stderr, "invalid -floor: %v\n", err)
		return 2
	}

	exceptions, err := readExceptions(*exceptionsPath)
	if err != nil {
		fmt.Fprintf(stderr, "read exceptions: %v\n", err)
		return 1
	}

	failures, err := readCoverageFailures(*profilePath, *defaultFloor, exceptions)
	if err != nil {
		fmt.Fprintf(stderr, "check coverage: %v\n", err)
		return 1
	}
	if len(failures) > 0 {
		writeFailures(stderr, failures)
		return 1
	}

	fmt.Fprintln(stdout, "coverage meets configured package and file floors")
	return 0
}

func readExceptions(path string) (map[string]float64, error) {
	if path == "" {
		return map[string]float64{}, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	exceptions, parseErr := parseExceptions(file)
	closeErr := file.Close()
	if parseErr != nil {
		return nil, parseErr
	}
	if closeErr != nil {
		return nil, closeErr
	}

	return exceptions, nil
}

func readCoverageFailures(profilePath string, defaultFloor float64, exceptions map[string]float64) ([]coverageFailure, error) {
	file, err := os.Open(profilePath)
	if err != nil {
		return nil, err
	}

	failures, parseErr := checkCoverage(file, defaultFloor, exceptions)
	closeErr := file.Close()
	if parseErr != nil {
		return nil, parseErr
	}
	if closeErr != nil {
		return nil, closeErr
	}

	return failures, nil
}

func checkCoverage(profile io.Reader, defaultFloor float64, exceptions map[string]float64) ([]coverageFailure, error) {
	if err := validateFloor(defaultFloor); err != nil {
		return nil, err
	}

	stats, err := parseCoverProfile(profile)
	if err != nil {
		return nil, err
	}

	packages := make([]string, 0, len(stats.packages))
	for packagePath := range stats.packages {
		packages = append(packages, packagePath)
	}
	sort.Strings(packages)

	var failures []coverageFailure
	for _, packagePath := range packages {
		floor := defaultFloor
		if exceptionFloor, ok := exceptions[packagePath]; ok {
			floor = exceptionFloor
		}

		coverage := stats.packages[packagePath].percent()
		if coverage < floor {
			failures = append(failures, coverageFailure{
				Package:  packagePath,
				Coverage: coverage,
				Floor:    floor,
			})
		}
	}

	files := make([]string, 0)
	for target := range exceptions {
		if strings.HasSuffix(target, ".go") {
			files = append(files, target)
		}
	}
	sort.Strings(files)
	for _, filename := range files {
		fileStats, ok := stats.files[filename]
		if !ok {
			return nil, fmt.Errorf("coverage floor target %q not found in profile", filename)
		}
		coverage := fileStats.percent()
		floor := exceptions[filename]
		if coverage < floor {
			failures = append(failures, coverageFailure{
				Package:  filename,
				Coverage: coverage,
				Floor:    floor,
			})
		}
	}

	return failures, nil
}

func parseCoverProfile(profile io.Reader) (profileStats, error) {
	scanner := bufio.NewScanner(profile)
	stats := profileStats{
		packages: make(map[string]packageStats),
		files:    make(map[string]packageStats),
	}

	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if lineNumber == 1 && strings.HasPrefix(line, "mode:") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) != 3 {
			return profileStats{}, fmt.Errorf("coverprofile line %d: want file range, statements, and count", lineNumber)
		}

		filename, ok := profileFilename(fields[0])
		if !ok {
			return profileStats{}, fmt.Errorf("coverprofile line %d: invalid file range %q", lineNumber, fields[0])
		}
		if isExcluded(filename) {
			continue
		}

		statements, err := strconv.Atoi(fields[1])
		if err != nil {
			return profileStats{}, fmt.Errorf("coverprofile line %d: invalid statement count %q: %w", lineNumber, fields[1], err)
		}
		if statements < 0 {
			return profileStats{}, fmt.Errorf("coverprofile line %d: statement count must be non-negative", lineNumber)
		}

		count, err := strconv.Atoi(fields[2])
		if err != nil {
			return profileStats{}, fmt.Errorf("coverprofile line %d: invalid coverage count %q: %w", lineNumber, fields[2], err)
		}
		if count < 0 {
			return profileStats{}, fmt.Errorf("coverprofile line %d: coverage count must be non-negative", lineNumber)
		}

		packagePath := packageDir(filename)
		packageCoverage := stats.packages[packagePath]
		packageCoverage.total += statements
		if count > 0 {
			packageCoverage.covered += statements
		}
		stats.packages[packagePath] = packageCoverage

		fileCoverage := stats.files[filename]
		fileCoverage.total += statements
		if count > 0 {
			fileCoverage.covered += statements
		}
		stats.files[filename] = fileCoverage
	}
	if err := scanner.Err(); err != nil {
		return profileStats{}, err
	}

	return stats, nil
}

func profileFilename(fileRange string) (string, bool) {
	filename, _, ok := strings.Cut(fileRange, ":")
	return filename, ok && filename != ""
}

func packageDir(filename string) string {
	index := strings.LastIndex(filename, "/")
	if index < 0 {
		return "."
	}

	return filename[:index]
}

func isExcluded(filename string) bool {
	return strings.HasSuffix(filename, "_templ.go") ||
		strings.Contains(filename, "/internal/store/sqlc/") ||
		strings.Contains(filename, "/internal/database/sqlc/")
}

func parseExceptions(exceptions io.Reader) (map[string]float64, error) {
	scanner := bufio.NewScanner(exceptions)
	floors := make(map[string]float64)

	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line, _, _ := strings.Cut(scanner.Text(), "#")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("exceptions line %d: want package path and floor", lineNumber)
		}

		floor, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return nil, fmt.Errorf("exceptions line %d: invalid floor %q: %w", lineNumber, fields[1], err)
		}
		if err := validateFloor(floor); err != nil {
			return nil, fmt.Errorf("exceptions line %d: invalid floor %q: %w", lineNumber, fields[1], err)
		}

		floors[fields[0]] = floor
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return floors, nil
}

func validateFloor(floor float64) error {
	switch {
	case math.IsNaN(floor), math.IsInf(floor, 0):
		return errors.New("floor must be finite")
	case floor < 0 || floor > 100:
		return errors.New("floor must be between 0 and 100")
	default:
		return nil
	}
}

func writeFailures(output io.Writer, failures []coverageFailure) {
	fmt.Fprintln(output, "coverage below floor:")
	for _, failure := range failures {
		fmt.Fprintf(output, "  %s: %.1f%% below %.1f%%\n", failure.Package, failure.Coverage, failure.Floor)
	}
}
