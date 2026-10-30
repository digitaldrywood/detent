package hubserver

import (
	"strings"
	"unicode/utf8"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

func registerNativeSearchSnippet(tls *libc.TLS, database, module uintptr) int32 {
	name, err := libc.CString("native_search_snippet")
	if err != nil {
		return sqlite3.SQLITE_NOMEM
	}
	defer libc.Xfree(tls, name)
	return sqlite3.Xsqlite3_create_function_v2(tls, database, name, 3, sqlite3.SQLITE_UTF8|sqlite3.SQLITE_DETERMINISTIC, module, nativeSearchCallback(nativeSearchSnippet), 0, 0, 0)
}

func nativeSearchSnippet(tls *libc.TLS, context uintptr, _ int32, arguments uintptr) {
	values := nativeSearchMemory[[3]uintptr](arguments)
	input := make([]string, len(values))
	for i, value := range values {
		text := sqlite3.Xsqlite3_value_text(tls, value)
		input[i] = string(libc.GoBytes(text, int(sqlite3.Xsqlite3_value_bytes(tls, value))))
	}
	storage := tls.Alloc(int(unsafe.Sizeof(uintptr(0))))
	defer tls.Free(int(unsafe.Sizeof(uintptr(0))))
	module := sqlite3.Xsqlite3_user_data(tls, context)
	if code := nativeSearchTokenizerCreate(tls, module, 0, 0, storage); code != sqlite3.SQLITE_OK {
		sqlite3.Xsqlite3_result_error_code(tls, context, code)
		return
	}
	tokenizer := *nativeSearchMemory[uintptr](storage)
	defer nativeSearchTokenizerDelete(tls, tokenizer)
	instance := nativeSearchMemory[nativeSearchTokenizer](tokenizer)
	base := *(*func(*libc.TLS, uintptr, uintptr, int32, uintptr, int32, uintptr) int32)(unsafe.Pointer(&instance.module.base.FxTokenize))
	tokenize := func(content string, flags int32, visit func(string, int, int)) int32 {
		text, err := libc.CString(content)
		if err != nil {
			return sqlite3.SQLITE_NOMEM
		}
		defer libc.Xfree(tls, text)
		callback := func(_ *libc.TLS, _ uintptr, _ int32, token uintptr, length, start, end int32) int32 {
			visit(string(libc.GoBytes(token, int(length))), int(start), int(end))
			return sqlite3.SQLITE_OK
		}
		address, release := nativeSearchPinCallback(callback)
		defer release()
		return base(tls, instance.base, 0, flags, text, int32(len(content)), address)
	}
	terms := map[string]bool{}
	for _, expression := range strings.Split(input[2], " OR ") {
		if code := tokenize(expression, sqlite3.FTS5_TOKENIZE_QUERY, func(term string, _, _ int) {
			terms[term] = strings.HasSuffix(expression, "*")
		}); code != sqlite3.SQLITE_OK {
			sqlite3.Xsqlite3_result_error_code(tls, context, code)
			return
		}
	}
	type span struct {
		start, end int
		terms      []string
	}
	var content string
	var matches []span
	coverage := -1
	anchor, count := 0, 0
	for _, column := range input[:2] {
		var spans []span
		if code := tokenize(column, sqlite3.FTS5_TOKENIZE_DOCUMENT, func(token string, start, end int) {
			var matched []string
			for term, prefix := range terms {
				if token == term || prefix && strings.HasPrefix(token, term) {
					matched = append(matched, term)
				}
			}
			if len(matched) > 0 {
				spans = append(spans, span{start, end, matched})
			}
		}); code != sqlite3.SQLITE_OK {
			sqlite3.Xsqlite3_result_error_code(tls, context, code)
			return
		}
		if coverage < 0 {
			content, coverage = column, 0
		}
		covered := map[string]int{}
		right := 0
		for left, match := range spans {
			for right < len(spans) && (right == left || spans[right].end-match.start <= 180) {
				for _, term := range spans[right].terms {
					covered[term]++
				}
				right++
			}
			if len(covered) > coverage || len(covered) == coverage && right-left > count {
				content, matches, coverage = column, spans, len(covered)
				anchor, count = match.start, right-left
			}
			for _, term := range match.terms {
				covered[term]--
				if covered[term] == 0 {
					delete(covered, term)
				}
			}
		}
	}
	start := 0
	if len(matches) > 0 {
		start = max(0, anchor-60)
		for start > 0 && !utf8.RuneStart(content[start]) {
			start--
		}
	}
	runes := []rune(content[start:])
	end := start + len(string(runes[:min(240, len(runes))]))
	var snippet strings.Builder
	for _, match := range matches {
		if match.start < start || match.end > end {
			continue
		}
		snippet.WriteString(content[start:match.start])
		snippet.WriteByte('[')
		snippet.WriteString(content[match.start:match.end])
		snippet.WriteByte(']')
		start = match.end
	}
	snippet.WriteString(content[start:end])
	output, err := libc.CString(snippet.String())
	if err != nil {
		sqlite3.Xsqlite3_result_error_nomem(tls, context)
		return
	}
	defer libc.Xfree(tls, output)
	sqlite3.Xsqlite3_result_text(tls, context, output, int32(snippet.Len()), ^uintptr(0))
}
