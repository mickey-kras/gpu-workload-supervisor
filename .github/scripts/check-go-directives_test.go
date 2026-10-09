package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeGoFixture(t *testing.T, root, path, source string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRejectAcceptedGocognitDirectives(t *testing.T) {
	// Sixteen independent ifs exceed the CI complexity ceiling. The upstream
	// analyzer skips this function when any accepted directive precedes it.
	function := "func tooComplex(value bool) int { n := 0;" +
		strings.Repeat("if value { n++ };", 16) + "return n }\n"
	for _, directive := range []string{
		"//gocognit:ignore\n",
		"\t//gocognit:ignore\r\n",
		"//gocog\rnit:ig\rnore\n",
		"// Explanation\n//gocognit:ignore\n// More explanation\n",
		"// nosemgrep\n//gocognit:ignore\n",
		"//gocognit:ignore\n// nosemgrep: go.gocognit-suppression\n",
	} {
		t.Run(directive, func(t *testing.T) {
			root := t.TempDir()
			writeGoFixture(t, root, "internal/setup/fixture.go", "package fixture\n"+directive+function)
			if err := checkDirectives(root); err == nil || !strings.Contains(err.Error(), "suppressions are forbidden") {
				t.Fatalf("suppressed high-complexity production function was accepted: %v", err)
			}
		})
	}
}

func TestNonDirectivesAndExistingFixtureScope(t *testing.T) {
	root := t.TempDir()
	writeGoFixture(t, root, "internal/setup/safe.go", `package fixture
const quoted = "//gocognit:ignore"
const raw = `+"`//gocognit:ignore`"+`
// gocognit:ignore
func spaced() {}
/*gocognit:ignore*/
func block() {}
//gocognit:ignore trailing text
func trailing() {}
`)
	for _, path := range []string{
		"internal/setup/fixture_test.go", ".github/scripts/fixture.go",
		"docs/fixture.go", "clients/node_modules/fixture.go",
	} {
		writeGoFixture(t, root, path, "package fixture\n//gocognit:ignore\nfunc fixture() {}\n")
	}
	if err := checkDirectives(root); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidProductionGoFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeGoFixture(t, root, "internal/setup/fixture.go", "invalid Go syntax")
	if err := checkDirectives(root); err == nil {
		t.Fatal("invalid production source was accepted")
	}
}

func TestRejectProductionPositionsRemappedIntoExcludedScope(t *testing.T) {
	function := "func tooComplex(value bool) int { n := 0;" +
		strings.Repeat("if value { n++ };", 16) + "return n }\n"
	for _, directive := range []string{
		"//line docs/fixture.go:1\n",
		"//line .github/fixture.go:1:2\n",
		"//line node_modules/fixture.go:1\r\n",
		"/*line docs/fixture.go:1*/",
		"/*line .github/fixture.go:1:2*/",
		"/*line node_modules/fixture.go:1*/",
		"//line docs/fixture.go:1\n// nosemgrep\n",
	} {
		t.Run(directive, func(t *testing.T) {
			root := t.TempDir()
			writeGoFixture(t, root, "internal/setup/fixture.go", "package fixture\n"+directive+function)
			err := checkDirectives(root)
			if err == nil || !strings.Contains(err.Error(), "remaps production into an excluded Go path") {
				t.Fatalf("disguised production function was accepted: %v", err)
			}
			if !strings.Contains(err.Error(), "internal/setup/fixture.go:") {
				t.Fatalf("diagnostic lost the physical source location: %v", err)
			}
		})
	}
}

func TestLineDirectivesThatRetainAnalyzedScope(t *testing.T) {
	root := t.TempDir()
	writeGoFixture(t, root, "internal/setup/safe.go", `package fixture
//line generated.go:100
func mapped() {}
/*line generated.go:200:2*/func blockMapped() {}
const quoted = "//line docs/fixture.go:1"
`)
	if err := checkDirectives(root); err != nil {
		t.Fatal(err)
	}
}
