package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/buger/jsonparser"
)

func replaceProjectConfigurationEntry(document, project, raw string) (string, error) {
	data := []byte(document)
	if !json.Valid(data) {
		return "", errors.New("invalid project configuration map")
	}
	if strings.TrimSpace(document) == "null" {
		data, document = []byte("{}"), "{}"
	}
	var start, end int
	nonempty := false
	err := jsonparser.ObjectEach(data, func(key, value []byte, kind jsonparser.ValueType, offset int) error {
		nonempty = true
		if string(key) == project {
			end = offset
			start = offset - len(value)
			if kind == jsonparser.String {
				start -= 2
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if end != 0 {
		return document[:start] + raw + document[end:], nil
	}
	key, err := json.Marshal(project)
	if err != nil {
		return "", err
	}
	closing := strings.LastIndexByte(document, '}')
	separator := ""
	if nonempty {
		separator = ","
	}
	return document[:closing] + separator + string(key) + ":" + raw + document[closing:], nil
}

func projectConfigurationEntry(document, project string) (sql.NullString, error) {
	var projects map[string]json.RawMessage
	if err := json.Unmarshal([]byte(document), &projects); err != nil {
		return sql.NullString{}, err
	}
	raw, found := projects[project]
	if !found || string(raw) == "null" {
		return sql.NullString{}, nil
	}
	return sql.NullString{String: string(raw), Valid: true}, nil
}

func readProjectConfigurationEntry(ctx context.Context, q nativeQueryer, scope nativeScope, runner string) (sql.NullString, error) {
	var document string
	if err := q.QueryRowContext(ctx, "SELECT project_configuration_json FROM runner_identities WHERE organization_id=? AND id=?", scope.organization, runner).Scan(&document); err != nil {
		return sql.NullString{}, err
	}
	return projectConfigurationEntry(document, string(scope.project))
}
