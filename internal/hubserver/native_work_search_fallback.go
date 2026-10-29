package hubserver

import (
	"math"
	"strings"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

func registerNativeSearchFallback(tls *libc.TLS, database, module uintptr) int32 {
	for _, function := range []struct {
		name     string
		args     int32
		callback func(*libc.TLS, uintptr, int32, uintptr)
	}{
		{"native_search_tokens", 2, nativeSearchTokens},
		{"native_search_bm25", 5, nativeSearchBM25},
	} {
		name, err := libc.CString(function.name)
		if err != nil {
			return sqlite3.SQLITE_NOMEM
		}
		code := sqlite3.Xsqlite3_create_function_v2(tls, database, name, function.args, sqlite3.SQLITE_UTF8|sqlite3.SQLITE_DETERMINISTIC, module, nativeSearchCallback(function.callback), 0, 0, 0)
		libc.Xfree(tls, name)
		if code != sqlite3.SQLITE_OK {
			return code
		}
	}
	return sqlite3.SQLITE_OK
}

func nativeSearchTokens(tls *libc.TLS, context uintptr, _ int32, arguments uintptr) {
	values := nativeSearchMemory[[2]uintptr](arguments)
	input := make([]string, len(values))
	for i, value := range values {
		text := sqlite3.Xsqlite3_value_text(tls, value)
		input[i] = string(libc.GoBytes(text, int(sqlite3.Xsqlite3_value_bytes(tls, value))))
	}
	storage := tls.Alloc(int(unsafe.Sizeof(uintptr(0))))
	defer tls.Free(int(unsafe.Sizeof(uintptr(0))))
	if code := nativeSearchTokenizerCreate(tls, sqlite3.Xsqlite3_user_data(tls, context), 0, 0, storage); code != sqlite3.SQLITE_OK {
		sqlite3.Xsqlite3_result_error_code(tls, context, code)
		return
	}
	tokenizer := *nativeSearchMemory[uintptr](storage)
	defer nativeSearchTokenizerDelete(tls, tokenizer)
	instance := nativeSearchMemory[nativeSearchTokenizer](tokenizer)
	base := *(*func(*libc.TLS, uintptr, uintptr, int32, uintptr, int32, uintptr) int32)(unsafe.Pointer(&instance.module.base.FxTokenize))
	var tokens [2][]string
	for i, content := range input {
		text, err := libc.CString(content)
		if err != nil {
			sqlite3.Xsqlite3_result_error_nomem(tls, context)
			return
		}
		callback := func(_ *libc.TLS, _ uintptr, _ int32, token uintptr, length, _, _ int32) int32 {
			tokens[i] = append(tokens[i], string(libc.GoBytes(token, int(length))))
			return sqlite3.SQLITE_OK
		}
		address, release := nativeSearchPinCallback(callback)
		flags := int32(sqlite3.FTS5_TOKENIZE_DOCUMENT)
		if i == 1 {
			flags = sqlite3.FTS5_TOKENIZE_QUERY
		}
		code := base(tls, instance.base, 0, flags, text, int32(len(content)), address)
		release()
		libc.Xfree(tls, text)
		if code != sqlite3.SQLITE_OK {
			sqlite3.Xsqlite3_result_error_code(tls, context, code)
			return
		}
	}
	if input[1] == "" {
		sqlite3.Xsqlite3_result_int64(tls, context, int64(len(tokens[0])))
		return
	}
	count := int64(0)
	prefix := strings.HasSuffix(input[1], "*")
	if len(tokens[1]) > 0 {
		for start := 0; start+len(tokens[1]) <= len(tokens[0]); start++ {
			matched := true
			for offset, term := range tokens[1] {
				token := tokens[0][start+offset]
				prefixMatch := prefix && offset == len(tokens[1])-1 && strings.HasPrefix(token, term)
				if token != term && !prefixMatch {
					matched = false
					break
				}
			}
			if matched {
				count++
			}
		}
	}
	sqlite3.Xsqlite3_result_int64(tls, context, count)
}

func nativeSearchBM25(tls *libc.TLS, context uintptr, _ int32, arguments uintptr) {
	values := nativeSearchMemory[[5]uintptr](arguments)
	frequency := sqlite3.Xsqlite3_value_double(tls, values[0])
	length := sqlite3.Xsqlite3_value_double(tls, values[1])
	average := sqlite3.Xsqlite3_value_double(tls, values[2])
	documents := sqlite3.Xsqlite3_value_double(tls, values[3])
	matching := sqlite3.Xsqlite3_value_double(tls, values[4])
	if frequency <= 0 || average <= 0 || matching <= 0 || matching > documents {
		sqlite3.Xsqlite3_result_double(tls, context, 0)
		return
	}
	idf := math.Log((documents - matching + 0.5) / (matching + 0.5))
	if idf <= 0 {
		idf = 1e-6
	}
	score := -idf * frequency * 2.2 / (frequency + 1.2*(0.25+0.75*length/average))
	sqlite3.Xsqlite3_result_double(tls, context, score)
}
