package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

type result struct {
	Name     string `json:"name"`
	Status   int    `json:"status"`
	Expected int    `json:"expected"`
	MS       int64  `json:"ms"`
	Body     string `json:"body"`
	Error    string `json:"error"`
	Note     string `json:"note"`
	Board    bool   `json:"board"`
	Skipped  bool   `json:"skipped"`
}

func classify(r result) string {
	if r.Skipped {
		return "SKIPPED"
	}
	if r.Error != "" || r.Status == 0 || r.Status != r.Expected {
		return "FAIL"
	}
	if r.Body != "" && r.Expected < 400 {
		var value any
		if json.Unmarshal([]byte(r.Body), &value) != nil || failed(value, r.Board) {
			return "FAIL"
		}
	}
	return "PASS"
}

func failed(value any, board bool) bool {
	switch v := value.(type) {
	case map[string]any:
		if v["isError"] == true || v["error"] != nil {
			return true
		}
		for key, child := range v {
			if board && (key == "projection_pending" && child == true || (key == "code" || key == "status") && strings.Contains(fmt.Sprint(child), "pending")) {
				return true
			}
			if failed(child, board) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if failed(child, board) {
				return true
			}
		}
	}
	return false
}

func run() int {
	var rows []result
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 32<<20)).Decode(&rows); err != nil || len(rows) == 0 {
		fmt.Println("FAIL smoke helper: could not complete; check Node and Playwright installation")
		return 1
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "RESULT\tCHECK\tHTTP\tMS\tDETAIL / ERROR SAMPLE")
	failures := 0
	for _, row := range rows {
		state := classify(row)
		detail := row.Note
		if state == "FAIL" {
			failures++
			detail = row.Error + " " + row.Body
		}
		for _, key := range []string{arg(4), os.Getenv("SMOKE_RUNNER_CREDENTIAL_ENV")} {
			if secret := os.Getenv(key); secret != "" {
				detail = strings.ReplaceAll(detail, secret, "[REDACTED]")
			}
		}
		detail = strings.Join(strings.Fields(detail), " ")
		if len(detail) > 240 {
			detail = detail[:240]
		}
		fmt.Fprintf(writer, "%s\t%s\t%d\t%d\t%s\n", state, row.Name, row.Status, row.MS, detail)
	}
	if err := writer.Flush(); err != nil {
		return 1
	}
	fmt.Printf("%d checks, %d FAIL\n", len(rows), failures)
	if failures > 0 {
		return 1
	}
	return 0
}

func arg(n int) string {
	if len(os.Args) > n {
		return os.Args[n]
	}
	return ""
}

func main() { os.Exit(run()) }
