package localboard

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/dependencyline"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Comment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Record struct {
	Key          string    `json:"key"`
	Kind         string    `json:"kind"`
	SourceID     string    `json:"source_id"`
	Number       int       `json:"number"`
	Repository   string    `json:"repository,omitempty"`
	Title        string    `json:"title"`
	Priority     *int      `json:"priority,omitempty"`
	Body         string    `json:"body"`
	State        string    `json:"state"`
	Author       string    `json:"author"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Comments     []Comment `json:"comments"`
	Dependencies []string  `json:"dependencies"`
}

type Request struct {
	Record Record `json:"record"`
	DryRun bool   `json:"dry_run"`
}

type Result struct {
	SourceState  string                   `json:"source_state"`
	SourceID     string                   `json:"source_id"`
	WorkItemID   tracker.NativeWorkItemID `json:"work_item_id,omitempty"`
	Action       string                   `json:"action"`
	State        string                   `json:"state"`
	UnknownLane  bool                     `json:"unknown_lane,omitempty"`
	Comments     int                      `json:"comments"`
	Dependencies int                      `json:"dependencies"`
}

func SourceKey(store, project, kind, id string) string {
	raw := []byte{}
	for _, value := range []string{store, project, kind, id} {
		raw = strconv.AppendQuote(raw, value)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func Read(ctx context.Context, path, project, kind, store string, terminal []string) (records []Record, resultErr error) {
	if project == "" || strings.TrimSpace(store) == "" || kind != "local_sqlite" && kind != "github_local" {
		return nil, errors.New("source project, stable store ID and local_sqlite or github_local kind are required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("resolve local board path")
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	db, err := sql.Open("sqlite", uri.String()+"?mode=ro&_pragma=query_only(1)&_pragma=trusted_schema(0)")
	if err != nil {
		return nil, errors.New("open local board copy read-only")
	}
	defer func() {
		if err := db.Close(); err != nil {
			resultErr = errors.Join(resultErr, errors.New("close local board copy"))
		}
	}()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, errors.New("read local board copy")
	}
	defer tx.Rollback()
	columns, err := tx.QueryContext(ctx, "SELECT name FROM pragma_table_info('detent_work_items')")
	if err != nil {
		return nil, errors.New("read local board schema")
	}
	defer columns.Close()
	has := map[string]bool{}
	for columns.Next() {
		var name string
		if err := columns.Scan(&name); err != nil {
			return nil, errors.New("read local board columns")
		}
		has[name] = true
	}
	columnErr := errors.Join(columns.Err(), columns.Close())
	if columnErr != nil {
		return nil, errors.New("read local board columns")
	}
	numberColumn, githubColumn := "0", "0"
	if has["number"] {
		numberColumn = "number"
	}
	if has["github_issue_number"] {
		githubColumn = "github_issue_number"
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, identifier, title, description, priority, state, author_id, created_at, updated_at, metadata_json, `+numberColumn+`, `+githubColumn+` FROM detent_work_items WHERE project_id = ? ORDER BY id`, project)
	if err != nil {
		return nil, errors.New("unsupported local board schema")
	}
	defer rows.Close()
	all := []Record{}
	byRef := map[string]string{}
	for rows.Next() {
		record := Record{Kind: kind}
		var identifier, created, updated, metadata string
		var githubNumber int
		if err := rows.Scan(&record.SourceID, &identifier, &record.Title, &record.Body, &record.Priority, &record.State, &record.Author, &created, &updated, &metadata, &record.Number, &githubNumber); err != nil {
			return nil, errors.New("read local board item")
		}
		record.Key = SourceKey(store, project, kind, record.SourceID)
		if record.Number == 0 {
			if _, number, ok := strings.Cut(identifier, "#"); ok {
				record.Number = sourceNumber(number)
			}
		}
		record.CreatedAt, err = sourceTime(created)
		if err != nil {
			return nil, errors.New("invalid local board item creation timestamp")
		}
		record.UpdatedAt, err = sourceTime(updated)
		if err != nil {
			return nil, errors.New("invalid local board item update timestamp")
		}
		if record.UpdatedAt.Before(record.CreatedAt) {
			record.UpdatedAt = record.CreatedAt
		}
		if record.Author == "" {
			record.Author = "unavailable"
		}
		if kind == "github_local" {
			var values map[string]string
			if err := json.Unmarshal([]byte(metadata), &values); err != nil {
				return nil, errors.New("invalid local board GitHub metadata")
			}
			if githubNumber == 0 {
				githubNumber = sourceNumber(values["github_issue_number"])
			}
			repository, number, ok := strings.Cut(identifier, "#")
			if githubNumber == 0 {
				githubNumber = sourceNumber(number)
			}
			if !ok || repository == "" || githubNumber <= 0 {
				return nil, errors.New("local board GitHub identity is missing")
			}
			record.Repository, record.Number = repository, githubNumber
		}
		byRef[strings.ToLower(identifier)] = record.Key
		byRef["#"+strconv.Itoa(record.Number)] = record.Key
		all = append(all, record)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, errors.New("read local board items")
	}
	for _, record := range all {
		repository := record.Repository
		if repository == "" {
			repository = "local/board"
		}
		refs, err := dependencyline.References(record.Body, repository)
		if err != nil {
			return nil, errors.New("invalid local board dependency declaration")
		}
		for _, ref := range refs {
			key := byRef[strings.ToLower(ref)]
			if key == "" && strings.HasPrefix(ref, strings.ToLower(repository)+"#") {
				key = byRef[strings.TrimPrefix(ref, strings.ToLower(repository))]
			}
			if key == "" {
				return nil, errors.New("local board dependency does not resolve within source project")
			}
			record.Dependencies = append(record.Dependencies, key)
		}
		record.Comments, err = readComments(ctx, tx, project, record.SourceID)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return dependencyOrder(records, terminal)
}

func sourceTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Unix(0, 0).UTC(), nil
	}
	value, err := time.Parse(time.RFC3339Nano, raw)
	return value, err
}

func readComments(ctx context.Context, tx *sql.Tx, project, item string) ([]Comment, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, event_kind, body, payload_json, created_at FROM detent_work_item_events WHERE project_id=? AND item_id=? AND event_kind IN ('comment','comment_edit','comment_delete') ORDER BY id`, project, item)
	if err != nil {
		return nil, errors.New("read local board comments")
	}
	defer rows.Close()
	comments := map[string]Comment{}
	order := []string{}
	for rows.Next() {
		var id int64
		var kind, body, payload, created string
		if err := rows.Scan(&id, &kind, &body, &payload, &created); err != nil {
			return nil, errors.New("read local board comment")
		}
		timestamp, err := sourceTime(created)
		if err != nil {
			return nil, errors.New("invalid local board comment timestamp")
		}
		if kind == "comment" {
			key := strconv.FormatInt(id, 10)
			comments[key] = Comment{ID: key, Body: body, CreatedAt: timestamp, UpdatedAt: timestamp}
			order = append(order, key)
			continue
		}
		var values map[string]string
		if err := json.Unmarshal([]byte(payload), &values); err != nil {
			return nil, errors.New("invalid local board comment event")
		}
		key := values["comment_id"]
		if kind == "comment_delete" {
			delete(comments, key)
			continue
		}
		if comment, ok := comments[key]; ok {
			comment.Body, comment.UpdatedAt = body, timestamp
			comments[key] = comment
		}
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("read local board comments")
	}
	result := []Comment{}
	for _, id := range order {
		if comment, ok := comments[id]; ok {
			result = append(result, comment)
		}
	}
	return result, nil
}

func Validate(record Record) error {
	if len(record.Key) != 64 || record.Kind != "local_sqlite" && record.Kind != "github_local" || record.SourceID == "" || len(record.SourceID) > 500 || len(record.Author) > 200 || strings.TrimSpace(record.Author) == "" {
		return errors.New("invalid local board source identity")
	}
	if _, err := hex.DecodeString(record.Key); err != nil {
		return errors.New("invalid local board source key")
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) || record.UpdatedAt.After(time.Now().Add(time.Minute)) {
		return errors.New("invalid local board source timestamps")
	}
	if len(record.Comments) > 1000 || len(record.Dependencies) > 100 {
		return errors.New("local board item exceeds comment or dependency bound")
	}
	seen := map[string]bool{}
	for _, comment := range record.Comments {
		if comment.ID == "" || len(comment.ID) > 200 || seen[comment.ID] || strings.TrimSpace(comment.Body) == "" || len(comment.Body) > 64<<10 || comment.CreatedAt.IsZero() || comment.UpdatedAt.Before(comment.CreatedAt) || comment.UpdatedAt.After(time.Now().Add(time.Minute)) {
			return errors.New("invalid local board comment")
		}
		seen[comment.ID] = true
	}
	for _, key := range record.Dependencies {
		if len(key) != 64 {
			return errors.New("invalid local board dependency key")
		}
	}
	if record.Kind == "github_local" && (record.Number <= 0 || len(strings.Split(record.Repository, "/")) != 2) {
		return errors.New("invalid local board GitHub identity")
	}
	return nil
}

func dependencyOrder(records []Record, terminal []string) ([]Record, error) {
	byKey := map[string]Record{}
	for _, record := range records {
		byKey[record.Key] = record
	}
	visited, visiting := map[string]bool{}, map[string]bool{}
	result := []Record{}
	var visit func(string) error
	visit = func(key string) error {
		if visited[key] {
			return nil
		}
		record, ok := byKey[key]
		if !ok {
			return errors.New("local board dependency is missing")
		}
		if visiting[key] {
			return errors.New("local board dependencies contain a cycle")
		}
		visiting[key] = true
		for _, dependency := range record.Dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[key] = false
		visited[key] = true
		result = append(result, record)
		return nil
	}
	for _, record := range records {
		if slices.ContainsFunc(terminal, func(state string) bool {
			return strings.EqualFold(strings.TrimSpace(state), strings.TrimSpace(record.State))
		}) {
			continue
		}
		if err := visit(record.Key); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func sourceNumber(raw string) int {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return value
}
