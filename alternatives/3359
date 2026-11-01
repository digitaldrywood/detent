package storagecatalog

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Cell struct {
	Encoding string
	Data     []byte
	Null     bool
}

type Value struct {
	Null      bool
	Text      string
	Integer   int64
	Real      float64
	Boolean   bool
	Bytes     []byte
	Time      time.Time
	Histogram []int64
}

var decimalFloat = regexp.MustCompile(`^-?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)
var timestampFraction = regexp.MustCompile(`[.,]([0-9]+)`)

func (c Catalog) Decode(tableName, columnName string, cell Cell) (Value, error) {
	table, ok := c.Tables[tableName]
	if !ok {
		return Value{}, ErrUnmapped
	}
	column, ok := table.Columns[columnName]
	if !ok {
		return Value{}, ErrUnmapped
	}
	switch column.Disposition {
	case "pending":
		return Value{}, ErrPending
	case "rebuild", "retired":
		return Value{}, ErrValue
	}
	return decode(column, cell)
}

func decode(column Column, cell Cell) (Value, error) {
	if cell.Null {
		if len(cell.Data) != 0 || cell.Encoding != "" || column.Null != "preserve" {
			return Value{}, ErrValue
		}
		return Value{Null: true}, nil
	}
	if column.Codec == "bytes" {
		data, err := decodeBytes(cell)
		if err != nil {
			return Value{}, err
		}
		if len(data) == 0 {
			switch column.Empty {
			case "reject":
				return Value{}, ErrValue
			case "null":
				return Value{Null: true}, nil
			}
		}
		return Value{Bytes: data}, nil
	}
	if cell.Encoding != "utf8" || !utf8.Valid(cell.Data) || bytes.IndexByte(cell.Data, 0) >= 0 {
		return Value{}, ErrValue
	}
	text := string(cell.Data)
	if len(column.Allowed) != 0 && !slices.Contains(column.Allowed, text) {
		return Value{}, ErrValue
	}
	if text == "" {
		switch column.Empty {
		case "reject":
			return Value{}, ErrValue
		case "null":
			return Value{Null: true}, nil
		}
	}
	switch column.Codec {
	case "text", "object-text":
		return Value{Text: text, Bytes: bytes.Clone(cell.Data)}, nil
	case "object-json":
		if !json.Valid(cell.Data) {
			return Value{}, ErrValue
		}
		return Value{Bytes: bytes.Clone(cell.Data)}, nil
	case "int64", "bool01", "unix-seconds":
		if !decimalInteger(text) {
			return Value{}, ErrValue
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return Value{}, ErrValue
		}
		if column.Minimum != nil && value < *column.Minimum || column.Maximum != nil && value > *column.Maximum {
			return Value{}, ErrValue
		}
		if column.Codec == "bool01" {
			if text != "0" && text != "1" {
				return Value{}, ErrValue
			}
			return Value{Boolean: value == 1}, nil
		}
		if column.Codec == "unix-seconds" {
			instant := time.Unix(value, 0).UTC()
			if instant.Year() < 1 || instant.Year() > 9999 {
				return Value{}, ErrValue
			}
			return Value{Time: instant}, nil
		}
		return Value{Integer: value}, nil
	case "float64":
		value, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || !decimalFloat.MatchString(text) {
			return Value{}, ErrValue
		}
		if column.Nonnegative && value < 0 {
			return Value{}, ErrValue
		}
		return Value{Real: value}, nil
	case "timestamp":
		if fraction := timestampFraction.FindStringSubmatch(text); len(fraction) != 0 && len(fraction[1]) > 6 && strings.Trim(fraction[1][6:], "0") != "" {
			return Value{}, ErrValue
		}
		value, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || value.Year() < 1 || value.Year() > 9999 || value.Nanosecond()%1000 != 0 {
			return Value{}, ErrValue
		}
		return Value{Time: value.UTC()}, nil
	case "histogram":
		var entries []json.RawMessage
		if json.Unmarshal(cell.Data, &entries) != nil || len(entries) != 64 {
			return Value{}, ErrValue
		}
		values := make([]int64, len(entries))
		var total int64
		for i, entry := range entries {
			text := string(entry)
			value, err := strconv.ParseInt(text, 10, 64)
			if !decimalInteger(text) || err != nil || value < 0 || value > math.MaxInt64-total {
				return Value{}, ErrValue
			}
			values[i] = value
			total += value
		}
		return Value{Histogram: values}, nil
	}
	return Value{}, ErrValue
}

func decimalInteger(text string) bool {
	if text == "" {
		return false
	}
	digits := strings.TrimPrefix(text, "-")
	if digits == "" {
		return false
	}
	for _, ch := range digits {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func decodeBytes(cell Cell) ([]byte, error) {
	var data []byte
	var err error
	switch cell.Encoding {
	case "binary":
		data = bytes.Clone(cell.Data)
	case "hex":
		data, err = hex.DecodeString(string(cell.Data))
	case "base64":
		data, err = base64.StdEncoding.Strict().DecodeString(string(cell.Data))
		if err == nil && base64.StdEncoding.EncodeToString(data) != string(cell.Data) {
			return nil, ErrValue
		}
	case "sqlite-hex":
		text := string(cell.Data)
		if !strings.HasPrefix(text, "X'") || !strings.HasSuffix(text, "'") {
			return nil, ErrValue
		}
		data, err = hex.DecodeString(text[2 : len(text)-1])
	default:
		return nil, ErrValue
	}
	if err != nil {
		return nil, ErrValue
	}
	return data, nil
}
