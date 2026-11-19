package postgres

import (
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
)

func TestReportEventProjectionColumns(t *testing.T) {
	t.Parallel()
	var projection reportstore.EventProjection
	value := reflect.ValueOf(&projection).Elem()
	fields := make(map[string]reflect.Value, value.NumField())
	for i := range value.NumField() {
		fields[strings.ToLower(value.Type().Field(i).Name)] = value.Field(i)
	}
	typedColumns := make(map[string]reflect.Type)
	for field := range reflect.TypeFor[pgquery.ReportEventFact]().Fields() {
		if field.Name != "OrganizationID" && field.Name != "EventID" {
			typedColumns[strings.ToLower(field.Name)] = field.Type
		}
	}

	columns := strings.Split(reportEventProjectionColumns, ",")
	bindings := reportEventProjectionFields(&projection)
	if len(bindings) != len(columns) {
		t.Fatalf("projection bindings = %d for columns %v", len(bindings), columns)
	}
	for i, column := range columns {
		key := strings.ToLower(strings.ReplaceAll(column, "_", ""))
		field, ok := fields[key]
		if !ok {
			t.Errorf("column %s has no unused projection field", column)
			continue
		}
		if typedColumns[key] != field.Type() {
			t.Errorf("column %s has database type %v, want %v", column, typedColumns[key], field.Type())
		}
		if bindings[i] != field.Addr().Interface() {
			t.Errorf("column %s does not bind its projection field", column)
		}
		delete(fields, key)
		delete(typedColumns, key)
	}
	if len(fields) != 0 || len(typedColumns) != 0 {
		t.Errorf("unmapped projection fields %v or database columns %v", reflect.ValueOf(fields).MapKeys(), reflect.ValueOf(typedColumns).MapKeys())
	}
}
