package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Keep this identical to the pinned gocognit command's -ignore expression.
var excludedGoPaths = regexp.MustCompile(`(^|/)(\.github|docs|node_modules)/`)

// Match gocognit v1.3.0's parseDirective on function doc comments. Parsing
// comments also preserves its handling of carriage returns without mistaking
// string literals for directives. Semgrep suppression comments have no effect.
func checkDirectives(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".github", "docs", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		positions := token.NewFileSet()
		file, err := parser.ParseFile(positions, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			physical := positions.PositionFor(function.Pos(), false)
			adjusted := positions.Position(function.Pos())
			// gocognit filters its adjusted diagnostic filename. Both Go line
			// directive forms can otherwise disguise production as excluded code.
			if adjusted.Filename != physical.Filename && excludedGoPaths.MatchString(adjusted.Filename) {
				return fmt.Errorf("%s: function position remaps production into an excluded Go path", physical)
			}
			if function.Doc == nil {
				continue
			}
			for _, comment := range function.Doc.List {
				if comment.Text == "//gocognit:ignore" {
					return fmt.Errorf("%s: gocognit function suppressions are forbidden", positions.PositionFor(comment.Pos(), false))
				}
			}
		}
		return nil
	})
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: check-go-directives <source-root>")
		os.Exit(2)
	}
	if err := checkDirectives(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
