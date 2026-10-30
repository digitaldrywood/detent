package gotestreceipt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

type sourcePackage struct {
	Standard   bool
	ImportPath string
	Name       string
	Dir        string
	GoFiles    []string
	CgoFiles   []string
	SFiles     []string
	Error      *struct{ Err string }
}

func checkEligibility(ctx context.Context, binary string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-deps", "-test", "-json", ".")
	procgroup.Configure(ctx, cmd)
	var output bytes.Buffer
	cmd.Stdout = &output
	if err := procgroup.Start(cmd); err != nil {
		return fmt.Errorf("inspect test dependencies: %w", err)
	}
	if err := procgroup.Wait(cmd); err != nil {
		return fmt.Errorf("inspect test dependencies: %w", err)
	}
	decoder := json.NewDecoder(&output)
	count := 0
	sources := make(map[string][32]byte)
	for {
		var pkg sourcePackage
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("decode test dependencies: %w", err)
		}
		if pkg.Error != nil {
			return errors.New("test dependency unavailable")
		}
		if pkg.Standard || (pkg.Name == "main" && strings.HasSuffix(pkg.ImportPath, ".test")) {
			continue
		}
		count++
		if len(pkg.CgoFiles) != 0 || len(pkg.SFiles) != 0 {
			return errors.New("native test dependencies are not observable")
		}
		for _, name := range pkg.GoFiles {
			path := filepath.Join(pkg.Dir, name)
			source, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read test source: %w", err)
			}
			sources[path] = sha256.Sum256(source)
			file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
			if err != nil {
				return fmt.Errorf("inspect test source: %w", err)
			}
			if err := checkSource(file); err != nil {
				return fmt.Errorf("inspect %s/%s: %w", pkg.ImportPath, name, err)
			}
		}
	}
	if count == 0 {
		return errors.New("test sources unavailable")
	}
	if err := matchCompiledSources(ctx, binary); err != nil {
		return err
	}
	for path, digest := range sources {
		source, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("recheck test source: %w", err)
		}
		if sha256.Sum256(source) != digest {
			return errors.New("test source changed during eligibility inspection")
		}
	}
	return nil
}

func matchCompiledSources(ctx context.Context, binary string) (err error) {
	dir, err := os.MkdirTemp("", "detent-test-source-*")
	if err != nil {
		return fmt.Errorf("prepare test source verification: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate source verifier: %w", err)
	}
	wrapper, err := Install(dir, executable)
	if err != nil {
		return err
	}
	observed := filepath.Join(dir, "binary-digest")
	cmd := exec.CommandContext(ctx, "go", "test", "-exec="+wrapper+" --detent-source-digest "+observed, ".")
	procgroup.Configure(ctx, cmd)
	if err := procgroup.Start(cmd); err != nil {
		return fmt.Errorf("compile inspected test sources: %w", err)
	}
	if err := procgroup.Wait(cmd); err != nil {
		return fmt.Errorf("compile inspected test sources: %w", err)
	}
	digest, err := os.ReadFile(observed)
	if err != nil {
		return fmt.Errorf("read inspected binary digest: %w", err)
	}
	original := sha256.New()
	if err := copyFile(original, binary); err != nil {
		return fmt.Errorf("hash supplied test binary: %w", err)
	}
	if bytes.Equal(original.Sum(nil), digest) {
		return nil
	}
	compiled := filepath.Join(dir, "verified.test")
	cmd = exec.CommandContext(ctx, "go", "test", "-c", "-o", compiled, ".")
	procgroup.Configure(ctx, cmd)
	if err := procgroup.Start(cmd); err != nil {
		return fmt.Errorf("compile retained test binary: %w", err)
	}
	if err := procgroup.Wait(cmd); err != nil {
		return fmt.Errorf("compile retained test binary: %w", err)
	}
	rebuilt := sha256.New()
	if err := copyFile(rebuilt, compiled); err != nil {
		return fmt.Errorf("hash inspected test binary: %w", err)
	}
	if !bytes.Equal(original.Sum(nil), rebuilt.Sum(nil)) {
		return errors.New("test binary does not match inspected sources and build settings")
	}
	return nil
}

func checkSource(file *ast.File) error {
	imports := make(map[string]string)
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return fmt.Errorf("inspect import: %w", err)
		}
		name := filepath.Base(path)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "." {
			return errors.New("dot imports prevent input observation")
		}
		imports[name] = path
		if unobservableImport(path) {
			return fmt.Errorf("test dependency %s has unobservable inputs", path)
		}
	}
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.VAR {
			for _, spec := range gen.Specs {
				for _, expr := range spec.(*ast.ValueSpec).Values {
					if !observableInitializer(expr, imports) {
						return errors.New("variable initialization may read outside the input log")
					}
				}
			}
		}
	}
	var refusal error
	ast.Inspect(file, func(node ast.Node) bool {
		if refusal != nil {
			return false
		}
		switch n := node.(type) {
		case *ast.FuncDecl:
			if n.Name.Name == "init" || n.Name.Name == "TestMain" {
				allowed := make(map[string]bool)
				if n.Name.Name == "TestMain" && n.Type.Params != nil && len(n.Type.Params.List) == 1 {
					param := n.Type.Params.List[0]
					if ptr, ok := param.Type.(*ast.StarExpr); ok {
						if selector, ok := ptr.X.(*ast.SelectorExpr); ok {
							if id, ok := selector.X.(*ast.Ident); ok && imports[id.Name] == "testing" && selector.Sel.Name == "M" && len(param.Names) == 1 {
								allowed[param.Names[0].Name+".Run"] = true
							}
						}
					}
				}
				if !observableCalls(n.Body, imports, allowed) {
					refusal = errors.New("test initialization runs outside the input log")
				}
			}
		case *ast.SelectorExpr:
			if n.Sel.Name == "Local" {
				refusal = errors.New("test reads an unobserved timezone")
				return false
			}
			id, ok := n.X.(*ast.Ident)
			if !ok {
				break
			}
			path := imports[id.Name]
			if path == "time" {
				switch n.Sel.Name {
				case "Now", "Since", "Until", "Sleep", "After", "AfterFunc", "NewTimer", "NewTicker", "Tick", "Local", "LoadLocation", "Parse", "ParseInLocation":
					refusal = errors.New("test reads an unobserved clock")
				}
			}
			if path == "context" && (strings.Contains(n.Sel.Name, "Timeout") || strings.Contains(n.Sel.Name, "Deadline")) {
				refusal = errors.New("test reads an unobserved deadline")
			}
			if path == "os" {
				switch n.Sel.Name {
				case "Getenv", "LookupEnv", "Environ", "Setenv", "Unsetenv", "Clearenv", "ExpandEnv", "Expand", "ReadFile", "WriteFile", "Open", "OpenFile", "Create", "CreateTemp", "MkdirTemp", "Mkdir", "MkdirAll", "Remove", "RemoveAll", "Rename", "Stat", "Lstat", "ReadDir", "Getwd", "Chdir", "TempDir", "IsNotExist", "IsExist", "IsPermission", "ErrNotExist", "ErrExist", "ErrPermission", "ErrClosed", "PathError", "LinkError", "File", "FileInfo", "FileMode", "DirEntry", "PathSeparator", "PathListSeparator", "DevNull", "O_RDONLY", "O_WRONLY", "O_RDWR", "O_APPEND", "O_CREATE", "O_EXCL", "O_SYNC", "O_TRUNC", "ModeDir", "ModeSymlink", "ModePerm", "Exit":
				default:
					refusal = errors.New("test reads unobserved process or host state")
				}
			}
		}
		return refusal == nil
	})
	return refusal
}

func unobservableImport(path string) bool {
	switch path {
	case "embed", "go/ast", "go/token", "testing/fstest", "bufio", "crypto/sha256", "crypto/sha512", "crypto/md5", "crypto/sha1", "crypto/aes", "crypto/des", "crypto/cipher", "crypto/hmac", "crypto/subtle", "encoding", "image/color", "bytes", "cmp", "context", "errors", "fmt", "io", "io/fs", "iter", "maps", "math", "math/bits", "math/big", "os", "path", "path/filepath", "reflect", "regexp", "regexp/syntax", "slices", "sort", "strconv", "strings", "sync", "sync/atomic", "testing", "testing/synctest", "time", "unicode", "unicode/utf8", "unicode/utf16", "net/url":
		return false
	}
	for _, prefix := range []string{"encoding/", "compress/", "hash/"} {
		if strings.HasPrefix(path, prefix) {
			return false
		}
	}
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

func observableInitializer(expr ast.Expr, imports map[string]string) bool {
	return observableCalls(expr, imports, nil)
}

func observableCalls(node ast.Node, imports map[string]string, allowed map[string]bool) bool {
	safe := true
	ast.Inspect(node, func(node ast.Node) bool {
		if !safe {
			return false
		}
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch function := call.Fun.(type) {
		case *ast.SelectorExpr:
			id, ok := function.X.(*ast.Ident)
			if !ok {
				safe = false
				break
			}
			if allowed[id.Name+"."+function.Sel.Name] {
				return true
			}
			if id.Obj != nil {
				safe = false
				break
			}
			switch imports[id.Name] + "." + function.Sel.Name {
			case "os.Getenv", "os.LookupEnv", "os.Environ", "os.Setenv", "os.Unsetenv", "os.Clearenv", "os.Exit", "errors.New", "regexp.MustCompile", "regexp.Compile", "strings.NewReplacer", "reflect.TypeOf", "time.Duration":
			default:
				safe = false
			}
		default:
			safe = false
		}
		return safe
	})
	return safe
}
