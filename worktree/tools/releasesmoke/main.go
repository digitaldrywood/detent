package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
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
	if v, ok := value.(map[string]any); ok {
		if v["isError"] == true || v["error"] != nil || failed(v["result"], false) || failed(v["structuredContent"], false) {
			return true
		}
	}
	return board && projectionPending(value)
}

func projectionPending(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if key == "projection_pending" && child == true || (key == "code" || key == "status") && strings.Contains(fmt.Sprint(child), "pending") {
				return true
			}
			if projectionPending(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if projectionPending(child) {
				return true
			}
		}
	}
	return false
}

func report(input io.Reader, output io.Writer) int {
	var rows []result
	if err := json.NewDecoder(io.LimitReader(input, 32<<20)).Decode(&rows); err != nil || len(rows) == 0 {
		fmt.Fprintln(output, "FAIL smoke helper: could not complete; check Node and Playwright installation")
		return 1
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "RESULT\tCHECK\tHTTP\tMS\tDETAIL")
	failures := 0
	for _, row := range rows {
		state := classify(row)
		detail := reportNote(row.Note)
		if state == "FAIL" {
			failures++
			switch {
			case row.Status != row.Expected || row.Status == 0:
				detail = "unexpected or missing HTTP status"
			case row.Error != "":
				detail = "journey failed"
			default:
				detail = "response validation failed"
			}
		}
		fmt.Fprintf(writer, "%s\t%s\t%d\t%d\t%s\n", state, reportName(row.Name), row.Status, row.MS, detail)
	}
	if err := writer.Flush(); err != nil {
		return 1
	}
	fmt.Fprintf(output, "%d checks, %d FAIL\n", len(rows), failures)
	if failures > 0 {
		return 1
	}
	return 0
}

func reportName(name string) string {
	switch name {
	case "smoke setup", "board API", "project lanes", "project lane", "board counts", "board status",
		"MCP session", "initialize", "initialized", "tools/list", "work_list no query", "work_list with query",
		"work_item", "list_comments", "recent Change", "list_changes", "get_change", "issue lifecycle",
		"file_issue", "reconcile file_issue with same request_id", "duplicate candidate", "file_issue confirmed disposable run",
		"add_comment", "edit_item", "move_item", "read before move", "read before cancel", "cancel disposable issue",
		"existing attempts", "missing item attempts", "missing attempt", "issue detail API", "Change detail API",
		"runner heartbeat identity dry read", "browser journeys", "browser board load 1", "browser board load 2",
		"browser board load 3", "board request count stable", "activity", "diagnostics", "issue detail page",
		"Change detail page", "close MCP session":
		return name
	}
	return "unrecognized check"
}

var countNote = regexp.MustCompile(`^(\d{1,6} (tools|board requests)|\d{1,6}/\d{1,6}/\d{1,6})$`)

func reportNote(note string) string {
	if countNote.MatchString(note) || note == "no test runner credential; no heartbeat was sent" {
		return note
	}
	return ""
}

func main() { os.Exit(report(os.Stdin, os.Stdout)) }
