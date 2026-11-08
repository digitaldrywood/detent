package invariants

import "strings"

type sqliteSQLToken struct {
	text  string
	quote byte
}

func sqliteSQLCounts(text string, counts map[string]int) {
	tokens := sqliteSQLTokens(text)
	for i, token := range tokens {
		next := func(offset int, text string) bool {
			return i+offset < len(tokens) && tokens[i+offset].text == text
		}
		qualified := i > 0 && tokens[i-1].text == "."
		if token.quote == 0 && !qualified {
			switch {
			case token.text == "julianday" && next(1, "("):
				counts["sql_julianday"]++
			case token.text == "datetime" && next(1, "(") && next(2, "now") && tokens[i+2].quote == '\'':
				counts["sql_datetime_now"]++
			case strings.HasPrefix(token.text, "pragma_") && next(1, "("):
				counts["sql_pragma_table"]++
			case token.text == "pragma" && i+1 < len(tokens) && (i == 0 || tokens[i-1].text == ";" || tokens[i-1].text == "explain") && (i+2 == len(tokens) || next(2, "(") || next(2, "=") || next(2, ".") || next(2, ";")):
				counts["sql_pragma"]++
			case token.text == "insert" && next(1, "or") && i+2 < len(tokens):
				switch tokens[i+2].text {
				case "abort", "fail", "ignore", "replace", "rollback":
					counts["sql_insert_or"]++
				}
			}
		}
		if token.quote == '\'' || i == 0 {
			continue
		}
		switch token.text {
		case "sqlite_master", "sqlite_schema", "sqlite_temp_master", "sqlite_temp_schema":
			previous := tokens[i-1].text
			if qualified && i >= 3 && (tokens[i-2].text == "main" || tokens[i-2].text == "temp") {
				previous = tokens[i-3].text
			}
			switch previous {
			case "from", "join", "into", "update":
				counts["sql_schema"]++
			}
		}
	}
}

func sqliteSQLTokens(text string) []sqliteSQLToken {
	var tokens []sqliteSQLToken
	for i := 0; i < len(text); {
		switch {
		case strings.ContainsRune(" \t\r\n", rune(text[i])):
			i++
		case strings.HasPrefix(text[i:], "--"):
			if end := strings.IndexByte(text[i:], '\n'); end >= 0 {
				i += end + 1
			} else {
				i = len(text)
			}
		case strings.HasPrefix(text[i:], "/*"):
			i += 2
			for depth := 1; i < len(text) && depth > 0; {
				switch {
				case strings.HasPrefix(text[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(text[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
		case text[i] == '\'' || text[i] == '"' || text[i] == '`':
			quote := text[i]
			escaped := quote == '\'' && i > 0 && (text[i-1] == 'e' || text[i-1] == 'E') && (i < 2 || !sqliteSQLWord(text[i-2]))
			i++
			start := i
			for i < len(text) {
				if text[i] == '\\' && escaped && i+1 < len(text) {
					i += 2
					continue
				}
				if text[i] == quote {
					if i+1 < len(text) && text[i+1] == quote {
						i += 2
						continue
					}
					break
				}
				i++
			}
			tokens = append(tokens, sqliteSQLToken{text: strings.ToLower(text[start:i]), quote: quote})
			if i < len(text) {
				i++
			}
		case text[i] == '$':
			end := i + 1
			for end < len(text) && (text[end] == '_' || text[end] >= 'a' && text[end] <= 'z' || text[end] >= 'A' && text[end] <= 'Z' || end > i+1 && text[end] >= '0' && text[end] <= '9') {
				end++
			}
			if end < len(text) && text[end] == '$' {
				delimiter := text[i : end+1]
				if close := strings.Index(text[end+1:], delimiter); close >= 0 {
					i = end + 1 + close + len(delimiter)
					continue
				}
			}
			tokens = append(tokens, sqliteSQLToken{text: "$"})
			i++
		case sqliteSQLWord(text[i]):
			start := i
			for i < len(text) && sqliteSQLWord(text[i]) {
				i++
			}
			tokens = append(tokens, sqliteSQLToken{text: strings.ToLower(text[start:i])})
		default:
			tokens = append(tokens, sqliteSQLToken{text: text[i : i+1]})
			i++
		}
	}
	return tokens
}

func sqliteSQLWord(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}
