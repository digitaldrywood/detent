package workspace

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/markdownfence"
)

const mergeCommitSubjectLimit = 40

func mergeTargetCommitSubjects(ctx context.Context, directory, head, target string, conflicts []string) ([]string, error) {
	if len(conflicts) == 0 {
		return nil, nil
	}
	args := []string{"--literal-pathspecs", "log", "--format=%s", "--max-count=41", head + ".." + target, "--"}
	args = append(args, conflicts...)
	out, err := runGitAt(ctx, directory, args...)
	if err != nil {
		return nil, fmt.Errorf("read merge target commit subjects: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	subjects := strings.Split(strings.TrimSpace(out), "\n")
	if len(subjects) > mergeCommitSubjectLimit {
		subjects = append(subjects[:mergeCommitSubjectLimit], "[additional target commits truncated]")
	}
	return subjects, nil
}

type mergeGoDeclaration struct {
	key    string
	symbol string
	file   string
}

func missingMergeGoDeclarations(ctx context.Context, directory, head, target, prHead, description string) ([]string, error) {
	packages := make(map[string]bool)
	for _, parent := range []string{target, prHead} {
		out, err := runGitAt(ctx, directory, "diff", "--name-only", "--no-renames", "-z", parent, head, "--")
		if err != nil {
			return nil, fmt.Errorf("read merge resolution changed paths: %w", err)
		}
		for _, file := range strings.Split(out, "\x00") {
			if strings.HasSuffix(file, ".go") {
				packages[path.Dir(file)] = true
			}
		}
	}
	if len(packages) == 0 {
		return nil, nil
	}
	cache := make(map[string]*ast.File)
	resolved, err := mergeGoDeclarations(ctx, directory, head, packages, cache, true)
	if err != nil {
		return nil, err
	}
	base, err := runGitAt(ctx, directory, "merge-base", target, prHead)
	if err != nil {
		return nil, fmt.Errorf("read merge base: %w", err)
	}
	parents, err := mergeGoDeclarations(ctx, directory, strings.TrimSpace(base), packages, cache, false)
	if err != nil {
		return nil, err
	}
	for _, parent := range []string{target, prHead} {
		declarations, err := mergeGoDeclarations(ctx, directory, parent, packages, cache, false)
		if err != nil {
			return nil, err
		}
		for key := range parents {
			if _, exists := declarations[key]; !exists {
				delete(parents, key)
			}
		}
	}
	symbolCounts := make(map[string]int)
	for _, declaration := range parents {
		symbolCounts[declaration.symbol]++
	}
	instructions := mergeIntentInstructions(description)
	var findings []string
	for key, declaration := range parents {
		if _, exists := resolved[key]; exists {
			continue
		}
		if symbolCounts[declaration.symbol] == 1 && mergeRemovalRequested(instructions, declaration, resolved) {
			continue
		}
		findings = append(findings, "Go declaration missing from merge resolution: "+declaration.file+": "+declaration.symbol)
	}
	slices.Sort(findings)
	return findings, nil
}

func mergeGoDeclarations(ctx context.Context, directory, revision string, packages map[string]bool, cache map[string]*ast.File, requireParse bool) (map[string]mergeGoDeclaration, error) {
	out, err := runGitAt(ctx, directory, "ls-tree", "-r", "--format=%(objectname) %(path)", "-z", revision)
	if err != nil {
		return nil, fmt.Errorf("list merge tree Go files: %w", err)
	}
	declarations := make(map[string]mergeGoDeclaration)
	initCounts := make(map[string]int)
	for _, entry := range strings.Split(out, "\x00") {
		object, file, ok := strings.Cut(entry, " ")
		if !ok {
			continue
		}
		if !strings.HasSuffix(file, ".go") || !packages[path.Dir(file)] {
			continue
		}
		parsed := cache[object]
		if parsed == nil {
			source, err := runGitAt(ctx, directory, "cat-file", "blob", object)
			if err != nil {
				return nil, fmt.Errorf("read merge tree Go source: %w", err)
			}
			parsed, err = parser.ParseFile(token.NewFileSet(), "", source, parser.SkipObjectResolution|parser.ParseComments)
			if parsed != nil && ast.IsGenerated(parsed) {
				continue
			}
			if err != nil {
				if !requireParse {
					continue
				}
				return nil, fmt.Errorf("%w: cannot parse Go declarations in merge tree", ErrMergeResolutionInvalid)
			}
			cache[object] = parsed
		}
		for _, node := range parsed.Decls {
			function, ok := node.(*ast.FuncDecl)
			if !ok || function.Name.Name == "_" {
				continue
			}
			symbol := function.Name.Name
			if function.Recv != nil && len(function.Recv.List) > 0 {
				symbol = mergeReceiverName(function.Recv.List[0].Type) + "." + symbol
			}
			kind := "source"
			if strings.HasSuffix(file, "_test.go") {
				kind = "test"
			}
			key := path.Dir(file) + ":" + parsed.Name.Name + ":" + kind + ":" + symbol
			if symbol == "init" {
				initCounts[key]++
				key += fmt.Sprintf(":%d", initCounts[key])
			}
			declarations[key] = mergeGoDeclaration{key: key, symbol: symbol, file: file}
		}
	}
	return declarations, nil
}

func mergeReceiverName(expression ast.Expr) string {
	switch receiver := expression.(type) {
	case *ast.Ident:
		return receiver.Name
	case *ast.StarExpr:
		return mergeReceiverName(receiver.X)
	case *ast.IndexExpr:
		return mergeReceiverName(receiver.X)
	case *ast.IndexListExpr:
		return mergeReceiverName(receiver.X)
	default:
		return ""
	}
}

var mergeRemovalInstruction = regexp.MustCompile("(?im)^\\s*(?:[-*]\\s+)?(?:remove|delete|drop)\\s+(?:(?:the|obsolete)\\s+)?(?:(?:Go\\s+)?(?:function|method|test)\\s+)?`?([A-Za-z_][A-Za-z0-9_.]*)`?[.!]?\\s*$")
var mergeRenameInstruction = regexp.MustCompile("(?im)^\\s*(?:[-*]\\s+)?rename\\s+(?:(?:the|Go)\\s+)?(?:(?:function|method|test)\\s+)?`?([A-Za-z_][A-Za-z0-9_.]*)`?\\s+to\\s+`?([A-Za-z_][A-Za-z0-9_.]*)`?[.!]?\\s*$")

func mergeRemovalRequested(description string, declaration mergeGoDeclaration, resolved map[string]mergeGoDeclaration) bool {
	for _, match := range mergeRemovalInstruction.FindAllStringSubmatch(description, -1) {
		if match[1] == declaration.symbol {
			return true
		}
	}
	for _, match := range mergeRenameInstruction.FindAllStringSubmatch(description, -1) {
		if match[1] != declaration.symbol {
			continue
		}
		prefix := strings.TrimSuffix(declaration.key, declaration.symbol)
		if _, exists := resolved[prefix+match[2]]; exists {
			return true
		}
	}
	return false
}

func mergeIntentInstructions(description string) string {
	var lines []string
	var fence markdownfence.Fence
	for _, line := range strings.Split(description, "\n") {
		if fence.Consume(line) {
			continue
		}
		if fence == "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}
