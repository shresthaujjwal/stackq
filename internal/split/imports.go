package split

import (
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"stackq/internal/git"
)

func isGo(p string) bool { return strings.HasSuffix(p, ".go") }

func isJS(p string) bool {
	switch path.Ext(p) {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts":
		return true
	}
	return false
}

func goImports(src []byte) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ImportsOnly)
	if err != nil {
		return nil
	}
	var out []string
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err == nil {
			out = append(out, p)
		}
	}
	return out
}

var jsImportRe = regexp.MustCompile(`(?:import|export)\s[^'"]*?from\s*['"]([^'"]+)['"]` +
	`|import\s*['"]([^'"]+)['"]` +
	`|require\(\s*['"]([^'"]+)['"]\s*\)` +
	`|import\(\s*['"]([^'"]+)['"]\s*\)`)

func jsImports(src []byte) []string {
	var out []string
	for _, m := range jsImportRe.FindAllSubmatch(src, -1) {
		for _, g := range m[1:] {
			if len(g) > 0 {
				out = append(out, string(g))
				break
			}
		}
	}
	return out
}

type goModule struct{ dir, path string }

var moduleRe = regexp.MustCompile(`(?m)^module\s+"?([^\s"]+)"?`)

// loadGoModules finds every go.mod in the tree so imports in multi-module
// repos resolve to directories.
func loadGoModules(r *git.Repo, rev string) []goModule {
	files, err := r.ListFiles(rev)
	if err != nil {
		return nil
	}
	var mods []goModule
	for _, f := range files {
		if path.Base(f) != "go.mod" || strings.Contains(f, "vendor/") || strings.Contains(f, "testdata/") {
			continue
		}
		src, err := r.Show(rev, f)
		if err != nil {
			continue
		}
		if m := moduleRe.FindSubmatch(src); m != nil {
			mods = append(mods, goModule{dir: path.Dir(f), path: string(m[1])})
		}
	}
	sort.Slice(mods, func(i, j int) bool { return len(mods[i].path) > len(mods[j].path) })
	return mods
}

func goImportDir(mods []goModule, imp string) (string, bool) {
	for _, m := range mods {
		if imp == m.path || strings.HasPrefix(imp, m.path+"/") {
			return path.Join(m.dir, strings.TrimPrefix(imp, m.path)), true
		}
	}
	return "", false
}

// importTargets returns the repo-relative paths (directories for Go,
// extensionless file paths for JS/TS) that file imports at rev.
func importTargets(r *git.Repo, rev, file string, mods []goModule) []string {
	if !isGo(file) && !isJS(file) {
		return nil
	}
	src, err := r.Show(rev, file)
	if err != nil {
		return nil
	}
	var out []string
	if isGo(file) {
		for _, imp := range goImports(src) {
			if d, ok := goImportDir(mods, imp); ok {
				out = append(out, d)
			}
		}
		return out
	}
	for _, spec := range jsImports(src) {
		if strings.HasPrefix(spec, ".") {
			out = append(out, path.Clean(path.Join(path.Dir(file), spec)))
		}
	}
	return out
}
