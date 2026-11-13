package hubserver

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func openSQLParameterTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	_, db := projectRecordPostgresWithAdmin(t, 2)
	return db
}

func TestSQLParameterBinding(t *testing.T) {
	for _, test := range []struct {
		name, statement string
		args            []any
		wantArgs        []any
		wantSQL         string
		invalid         bool
	}{
		{name: "numbered reuse", statement: "SELECT ?2,?1,?2", args: []any{"first", "second"}, wantArgs: []any{"second", "first", "second"}, wantSQL: "SELECT ?,?,?"},
		{name: "quoted and commented markers", statement: "SELECT 'sql_rows(?)',? /* ? */ -- ?\n", args: []any{int64(2)}, wantArgs: []any{int64(2)}, wantSQL: "SELECT 'sql_rows(?)',? /* ? */ -- ?\n"},
		{name: "missing parameter", statement: "SELECT ?", invalid: true},
		{name: "unused parameter", statement: "SELECT 1", args: []any{1}, invalid: true},
		{name: "zero parameter index", statement: "SELECT ?0", args: []any{1}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			statement, args, err := hubapi.BindSQLParameters(test.statement, test.args)
			if test.invalid {
				if err == nil {
					t.Fatal("invalid parameters accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if statement != test.wantSQL || !reflect.DeepEqual(args, test.wantArgs) {
				t.Fatalf("bound statement=%q args=%v", statement, args)
			}
		})
	}
	if _, err := hubapi.SQLList(make([]string, hubapi.MaxSQLParameters+1)); err == nil {
		t.Fatal("unbounded list accepted")
	}
	list, err := hubapi.SQLList(make([]int64, hubapi.MaxSQLParameters))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := hubapi.BindSQLParameters("SELECT ? FROM sql_rows(?)", []any{1, list}); err == nil {
		t.Fatal("combined parameters exceed the statement bound")
	}
}
