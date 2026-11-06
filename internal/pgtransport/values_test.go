package pgtransport_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestNativeValues(t *testing.T) {
	_, config := fixture(t)
	pools := openPools(t, config, nil)
	tx := begin(t, pools, pgtransport.Options{ReadOnly: true})
	timestamp := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.FixedZone("example", 3600))
	normalized := pgtransport.Timestamp(timestamp)
	var got time.Time
	var missingText, missingTime, missingArray bool
	var text string
	var size int
	if err := tx.QueryRow(t.Context(), `SELECT $1::text IS NULL,$2::text,$3::timestamptz IS NULL,$4::timestamptz,$5::text[] IS NULL,cardinality($6::text[])`, pgtransport.Nullable("", false), pgtransport.Nullable("present", true), pgtransport.Nullable(timestamp, false), normalized, pgtransport.Array([]string(nil)), pgtransport.Array([]string{})).Scan(&missingText, &text, &missingTime, &got, &missingArray, &size); err != nil {
		t.Fatal(err)
	}
	if !missingText || text != "present" || !missingTime || !got.Equal(normalized) || got.Nanosecond() != 123456000 || !missingArray || size != 0 {
		t.Fatalf("textNull=%v text=%s timeNull=%v time=%s arrayNull=%v size=%d", missingText, text, missingTime, got, missingArray, size)
	}
	var array []string
	if err := tx.QueryRow(t.Context(), `SELECT $1::text[]`, pgtransport.Array([]string{"a,b", "quoted\"value", ""})).Scan(&array); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(array, []string{"a,b", "quoted\"value", ""}) {
		t.Fatalf("array=%q", array)
	}
	sqlNull, err := pgtransport.JSONB(nil)
	if err != nil {
		t.Fatal(err)
	}
	jsonNull, err := pgtransport.JSONB(json.RawMessage(`null`))
	if err != nil {
		t.Fatal(err)
	}
	var null bool
	var literal string
	if err := tx.QueryRow(t.Context(), `SELECT $1::jsonb IS NULL,$2::jsonb::text`, sqlNull, jsonNull).Scan(&null, &literal); err != nil || !null || literal != "null" {
		t.Fatalf("SQL NULL=%v JSON null=%s err=%v", null, literal, err)
	}
}
func TestJSONBRepresentability(t *testing.T) {
	_, config := fixture(t)
	pools := openPools(t, config, nil)
	for _, test := range []struct {
		name, stringValue string
		valid             bool
	}{
		{"object", `{"key":"value"}`, true},
		{"literal escaped NUL", `"\\u0000"`, true},
		{"paired surrogate", `"\uD83D\uDE00"`, true},
		{"escaped NUL", `"\u0000"`, false},
		{"NUL key", `{"\u0000":0}`, false},
		{"unpaired high", `"\uD800"`, false},
		{"unpaired low", `"\uDC00"`, false},
		{"invalid syntax", `{`, false},
		{"invalid UTF8", string([]byte{'"', 0xff, '"'}), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := json.RawMessage(test.stringValue)
			value, err := pgtransport.JSONB(input)
			if (err == nil) != test.valid {
				t.Fatalf("adapter valid=%v err=%v", test.valid, err)
			}
			tx := begin(t, pools, pgtransport.Options{ReadOnly: true})
			var result json.RawMessage
			serverErr := tx.QueryRow(t.Context(), `SELECT $1::jsonb`, input).Scan(&result)
			if (serverErr == nil) != test.valid {
				t.Fatalf("server valid=%v err=%v", test.valid, serverErr)
			}
			if test.valid && !reflect.DeepEqual(value, input) {
				t.Fatal("adapter changed valid JSONB")
			}
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestErrorClassification(t *testing.T) {
	_, config := fixture(t)
	pools := openPools(t, config, nil)
	for _, state := range []pgtransport.State{pgtransport.Serialization, pgtransport.Deadlock, pgtransport.Unique, pgtransport.ForeignKey, pgtransport.Check, pgtransport.LockTimeout, pgtransport.Cancel} {
		t.Run(string(state), func(t *testing.T) {
			tx := begin(t, pools, pgtransport.Options{})
			_, err := tx.Exec(t.Context(), `DO $$ BEGIN RAISE EXCEPTION USING ERRCODE='`+string(state)+`'; END $$`)
			wrapped := fmt.Errorf("execute: %w", errors.Join(errors.New("cleanup"), err))
			if pgtransport.SQLState(wrapped) != state || pgtransport.Classify(wrapped) != state {
				t.Fatalf("state=%s err=%v", state, err)
			}
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, fmt.Errorf("execute: %w", context.Canceled), errors.Join(errors.New("cleanup"), context.DeadlineExceeded)} {
		if pgtransport.SQLState(err) != "" || pgtransport.Classify(err) != pgtransport.Cancel {
			t.Fatalf("local cancellation=%v", err)
		}
	}
	mixed := errors.Join(context.Canceled, &pgconn.PgError{Code: string(pgtransport.Unique)})
	if pgtransport.Classify(mixed) != pgtransport.Unique {
		t.Fatal("lost primary server SQLSTATE")
	}
}
