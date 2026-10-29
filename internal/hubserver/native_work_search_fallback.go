package hubserver

import (
	"fmt"
	"math"
	"slices"
	"strconv"
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
		{"native_search_postings", 2, nativeSearchPostingText},
		{"native_search_key", 1, nativeSearchKey},
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

func nativeSearchLexemes(tls *libc.TLS, context uintptr, content string) ([]string, error) {
	storage := tls.Alloc(int(unsafe.Sizeof(uintptr(0))))
	defer tls.Free(int(unsafe.Sizeof(uintptr(0))))
	if code := nativeSearchTokenizerCreate(tls, sqlite3.Xsqlite3_user_data(tls, context), 0, 0, storage); code != sqlite3.SQLITE_OK {
		return nil, fmt.Errorf("create posting tokenizer: sqlite code %d", code)
	}
	tokenizer := *nativeSearchMemory[uintptr](storage)
	defer nativeSearchTokenizerDelete(tls, tokenizer)
	instance := nativeSearchMemory[nativeSearchTokenizer](tokenizer)
	base := *(*func(*libc.TLS, uintptr, uintptr, int32, uintptr, int32, uintptr) int32)(unsafe.Pointer(&instance.module.base.FxTokenize))
	text, err := libc.CString(content)
	if err != nil {
		return nil, err
	}
	defer libc.Xfree(tls, text)
	var tokens []string
	callback := func(_ *libc.TLS, _ uintptr, _ int32, token uintptr, length, _, _ int32) int32 {
		tokens = append(tokens, string(libc.GoBytes(token, int(length))))
		return sqlite3.SQLITE_OK
	}
	address, release := nativeSearchPinCallback(callback)
	defer release()
	if code := base(tls, instance.base, 0, sqlite3.FTS5_TOKENIZE_DOCUMENT, text, int32(len(content)), address); code != sqlite3.SQLITE_OK {
		return nil, fmt.Errorf("tokenize postings: sqlite code %d", code)
	}
	return tokens, nil
}

func nativeSearchResultText(tls *libc.TLS, context uintptr, value string) {
	text, err := libc.CString(value)
	if err != nil {
		sqlite3.Xsqlite3_result_error_nomem(tls, context)
		return
	}
	defer libc.Xfree(tls, text)
	sqlite3.Xsqlite3_result_text(tls, context, text, int32(len(value)), ^uintptr(0))
}

const nativeSearchPrefixLimit = 64

func nativeSearchPrefix(token string) string {
	count := 0
	for offset := range token {
		if count == nativeSearchPrefixLimit {
			return token[:offset]
		}
		count++
	}
	return token
}

func nativeSearchKey(tls *libc.TLS, context uintptr, _ int32, arguments uintptr) {
	value := *nativeSearchMemory[uintptr](arguments)
	input := string(libc.GoBytes(sqlite3.Xsqlite3_value_text(tls, value), int(sqlite3.Xsqlite3_value_bytes(tls, value))))
	tokens, err := nativeSearchLexemes(tls, context, input)
	if err != nil {
		sqlite3.Xsqlite3_result_error_code(tls, context, sqlite3.SQLITE_ERROR)
		return
	}
	key := ""
	if len(tokens) == 1 {
		key = "e" + tokens[0]
		if strings.HasSuffix(input, "*") {
			key = "p" + nativeSearchPrefix(tokens[0])
		}
	}
	nativeSearchResultText(tls, context, key)
}

func nativeSearchPostingText(tls *libc.TLS, context uintptr, _ int32, arguments uintptr) {
	values := nativeSearchMemory[[2]uintptr](arguments)
	frequencies := map[string]int{}
	for index, value := range values {
		tokens, err := nativeSearchLexemes(tls, context, string(libc.GoBytes(sqlite3.Xsqlite3_value_text(tls, value), int(sqlite3.Xsqlite3_value_bytes(tls, value)))))
		if err != nil {
			sqlite3.Xsqlite3_result_error_code(tls, context, sqlite3.SQLITE_ERROR)
			return
		}
		weight := 1
		if index == 0 {
			weight = 5
		}
		for _, token := range tokens {
			frequencies["e"+token] += weight
			prefix := nativeSearchPrefix(token)
			for offset := range prefix {
				if offset > 0 {
					frequencies["p"+prefix[:offset]] += weight
				}
			}
			frequencies["p"+prefix] += weight
		}
	}
	keys := make([]string, 0, len(frequencies))
	for key := range frequencies {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var output strings.Builder
	for _, key := range keys {
		output.WriteString(strconv.Itoa(frequencies[key]))
		output.WriteByte(':')
		output.WriteString(key)
		output.WriteByte(' ')
	}
	nativeSearchResultText(tls, context, output.String())
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
