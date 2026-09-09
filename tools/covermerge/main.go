package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("covermerge", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if len(flags.Args()) < 2 {
		fmt.Fprintln(stderr, "at least two disjoint set or atomic profiles are required")
		return 2
	}
	var output bytes.Buffer
	fmt.Fprintln(&output, "mode: set")
	files := make(map[string]int)
	for i, path := range flags.Args() {
		data, err := os.ReadFile(path)
		if err == nil {
			err = appendProfile(&output, data, i, files)
		}
		if err != nil {
			fmt.Fprintf(stderr, "merge coverage %s: %v\n", path, err)
			return 1
		}
	}
	if _, err := output.WriteTo(stdout); err != nil {
		fmt.Fprintf(stderr, "write coverage: %v\n", err)
		return 1
	}
	return 0
}

func appendProfile(output io.Writer, data []byte, index int, files map[string]int) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	if !scanner.Scan() || (scanner.Text() != "mode: atomic" && scanner.Text() != "mode: set") {
		return errors.New("missing or incompatible coverage mode")
	}
	mode := scanner.Text()
	blocks := 0
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return errors.New("invalid coverage block")
		}
		path, location, ok := strings.Cut(fields[0], ":")
		if !ok || path == "" || !validLocation(location) {
			return errors.New("invalid coverage location")
		}
		if _, err := strconv.ParseUint(fields[1], 10, 64); err != nil {
			return errors.New("invalid coverage count")
		}
		count, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil || (mode == "mode: set" && count > 1) {
			return errors.New("invalid coverage count")
		}
		if count > 0 {
			count = 1
		}
		if previous, exists := files[path]; exists && previous != index {
			return fmt.Errorf("overlapping profiles for %s", path)
		}
		files[path] = index
		if _, err := fmt.Fprintf(output, "%s %s %d\n", fields[0], fields[1], count); err != nil {
			return err
		}
		blocks++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if blocks == 0 {
		return errors.New("empty coverage profile")
	}
	return nil
}

func validLocation(location string) bool {
	start, end, ok := strings.Cut(location, ",")
	if !ok {
		return false
	}
	for _, position := range []string{start, end} {
		line, column, ok := strings.Cut(position, ".")
		if !ok {
			return false
		}
		for _, value := range []string{line, column} {
			if n, err := strconv.ParseUint(value, 10, 32); err != nil || n == 0 {
				return false
			}
		}
	}
	return true
}
