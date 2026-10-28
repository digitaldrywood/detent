package hubserver

import (
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

type nativeSearchTokenizerModule struct {
	base    sqlite3.Tfts5_tokenizer
	context uintptr
}

type nativeSearchTokenizer struct {
	module nativeSearchTokenizerModule
	base   uintptr
}

func nativeSearchCallback[T any](callback T) uintptr {
	return *(*uintptr)(unsafe.Pointer(&callback))
}

func nativeSearchMemory[T any](address uintptr) *T {
	var value T
	return (*T)(unsafe.Pointer(unsafe.SliceData(libc.GoBytes(address, int(unsafe.Sizeof(value))))))
}

func nativeSearchPinCallback[T any](callback T) (uintptr, func()) {
	var pinned runtime.Pinner
	pinned.Pin(&callback)
	address := *(*unsafe.Pointer)(unsafe.Pointer(&callback))
	pinned.Pin(address)
	return uintptr(address), pinned.Unpin
}

func registerNativeSearchTokenizer() error {
	tls := libc.NewTLS()
	defer tls.Close()
	if code := sqlite3.Xsqlite3_auto_extension(tls, nativeSearchCallback(nativeSearchTokenizerInit)); code != sqlite3.SQLITE_OK {
		return fmt.Errorf("register scoped search tokenizer: sqlite code %d", code)
	}
	return nil
}

func nativeSearchTokenizerInit(tls *libc.TLS, database, _, _ uintptr) int32 {
	sql, err := libc.CString("SELECT fts5(?1)")
	if err != nil {
		return sqlite3.SQLITE_NOMEM
	}
	defer libc.Xfree(tls, sql)
	pointerType, err := libc.CString("fts5_api_ptr")
	if err != nil {
		return sqlite3.SQLITE_NOMEM
	}
	defer libc.Xfree(tls, pointerType)
	storage := tls.Alloc(2 * int(unsafe.Sizeof(uintptr(0))))
	defer tls.Free(2 * int(unsafe.Sizeof(uintptr(0))))
	apiPointer := storage + unsafe.Sizeof(uintptr(0))
	*nativeSearchMemory[uintptr](apiPointer) = 0
	if code := sqlite3.Xsqlite3_prepare_v2(tls, database, sql, -1, storage, 0); code != sqlite3.SQLITE_OK {
		return code
	}
	statement := *nativeSearchMemory[uintptr](storage)
	defer sqlite3.Xsqlite3_finalize(tls, statement)
	if code := sqlite3.Xsqlite3_bind_pointer(tls, statement, 1, apiPointer, pointerType, 0); code != sqlite3.SQLITE_OK {
		return code
	}
	if code := sqlite3.Xsqlite3_step(tls, statement); code != sqlite3.SQLITE_ROW {
		return code
	}
	api := *nativeSearchMemory[uintptr](apiPointer)
	if api == 0 {
		return sqlite3.SQLITE_ERROR
	}
	module := sqlite3.Xsqlite3_malloc64(tls, uint64(unsafe.Sizeof(nativeSearchTokenizerModule{})))
	if module == 0 {
		return sqlite3.SQLITE_NOMEM
	}
	code := registerNativeSearchSnippet(tls, database, module)
	if code == sqlite3.SQLITE_OK {
		code = nativeSearchTokenizerRegister(tls, api, module)
	}
	if code != sqlite3.SQLITE_OK {
		sqlite3.Xsqlite3_free(tls, module)
	}
	return code
}

func nativeSearchTokenizerRegister(tls *libc.TLS, api, module uintptr) int32 {
	porter, err := libc.CString("porter")
	if err != nil {
		return sqlite3.SQLITE_NOMEM
	}
	defer libc.Xfree(tls, porter)
	name, err := libc.CString("native_scoped")
	if err != nil {
		return sqlite3.SQLITE_NOMEM
	}
	defer libc.Xfree(tls, name)
	methods := nativeSearchMemory[sqlite3.Tfts5_api](api)
	find := *(*func(*libc.TLS, uintptr, uintptr, uintptr, uintptr) int32)(unsafe.Pointer(&methods.FxFindTokenizer))
	if code := find(tls, api, porter, module+unsafe.Offsetof(nativeSearchTokenizerModule{}.context), module); code != sqlite3.SQLITE_OK {
		return code
	}
	tokenizer := tls.Alloc(int(unsafe.Sizeof(sqlite3.Tfts5_tokenizer{})))
	defer tls.Free(int(unsafe.Sizeof(sqlite3.Tfts5_tokenizer{})))
	*nativeSearchMemory[sqlite3.Tfts5_tokenizer](tokenizer) = sqlite3.Tfts5_tokenizer{
		FxCreate:   nativeSearchCallback(nativeSearchTokenizerCreate),
		FxDelete:   nativeSearchCallback(nativeSearchTokenizerDelete),
		FxTokenize: nativeSearchCallback(nativeSearchTokenizerTokenize),
	}
	create := *(*func(*libc.TLS, uintptr, uintptr, uintptr, uintptr, uintptr) int32)(unsafe.Pointer(&methods.FxCreateTokenizer))
	return create(tls, api, name, module, tokenizer, nativeSearchCallback(sqlite3.Xsqlite3_free))
}

func nativeSearchTokenizerCreate(tls *libc.TLS, module, arguments uintptr, count int32, output uintptr) int32 {
	tokenizer := sqlite3.Xsqlite3_malloc64(tls, uint64(unsafe.Sizeof(nativeSearchTokenizer{})))
	if tokenizer == 0 {
		return sqlite3.SQLITE_NOMEM
	}
	instance := nativeSearchMemory[nativeSearchTokenizer](tokenizer)
	instance.module = *nativeSearchMemory[nativeSearchTokenizerModule](module)
	create := *(*func(*libc.TLS, uintptr, uintptr, int32, uintptr) int32)(unsafe.Pointer(&instance.module.base.FxCreate))
	if code := create(tls, instance.module.context, arguments, count, tokenizer+unsafe.Offsetof(nativeSearchTokenizer{}.base)); code != sqlite3.SQLITE_OK {
		sqlite3.Xsqlite3_free(tls, tokenizer)
		return code
	}
	*nativeSearchMemory[uintptr](output) = tokenizer
	return sqlite3.SQLITE_OK
}

func nativeSearchTokenizerDelete(tls *libc.TLS, tokenizer uintptr) {
	instance := nativeSearchMemory[nativeSearchTokenizer](tokenizer)
	remove := *(*func(*libc.TLS, uintptr))(unsafe.Pointer(&instance.module.base.FxDelete))
	remove(tls, instance.base)
	sqlite3.Xsqlite3_free(tls, tokenizer)
}

func nativeSearchTokenizerTokenize(tls *libc.TLS, tokenizer, context uintptr, flags int32, text uintptr, length int32, emit uintptr) int32 {
	instance := nativeSearchMemory[nativeSearchTokenizer](tokenizer)
	tokenize := *(*func(*libc.TLS, uintptr, uintptr, int32, uintptr, int32, uintptr) int32)(unsafe.Pointer(&instance.module.base.FxTokenize))
	content := string(libc.GoBytes(text, int(length)))
	scope, _, scoped := strings.Cut(content, "_")
	if !scoped {
		return tokenize(tls, instance.base, context, flags, text, length, emit)
	}
	prefix := strings.ToLower(scope) + "_"
	offset := int32(len(prefix))
	callback := func(tls *libc.TLS, _ uintptr, tokenFlags int32, token uintptr, tokenLength, start, end int32) int32 {
		qualified := prefix + string(libc.GoBytes(token, int(tokenLength)))
		output, err := libc.CString(qualified)
		if err != nil {
			return sqlite3.SQLITE_NOMEM
		}
		defer libc.Xfree(tls, output)
		emitter := *(*func(*libc.TLS, uintptr, int32, uintptr, int32, int32, int32) int32)(unsafe.Pointer(&emit))
		return emitter(tls, context, tokenFlags, output, int32(len(qualified)), start+offset, end+offset)
	}
	address, release := nativeSearchPinCallback(callback)
	defer release()
	return tokenize(tls, instance.base, context, flags, text+uintptr(offset), length-offset, address)
}
