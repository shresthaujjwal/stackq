package split

import (
	"reflect"
	"strings"
	"testing"

	"stackq/internal/testrepo"
)

func names(p *Plan) []string {
	var out []string
	for _, s := range p.Slices {
		out = append(out, s.Name)
	}
	return out
}

func TestPlanFollowsImportsAndDeletions(t *testing.T) {
	r := testrepo.GoApp(t)
	p, err := Make(r, Options{Base: "main", Head: "feature", MaxLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	// deps first; types before core before the entrypoint that uses it;
	// the old package is deleted only after nothing imports it; docs last.
	want := []string{"deps", "internal/types", "internal/core", "cmd/app", "internal/old", "docs"}
	if got := names(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestPlanPacksByBudget(t *testing.T) {
	r := testrepo.GoApp(t)
	p, err := Make(r, Options{Base: "main", Head: "feature", MaxLines: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Slices) != 1 || len(p.Slices[0].Files) != 6 {
		t.Fatalf("expected one slice with all 6 files, got %v", names(p))
	}
}

func TestPlanRulesArePinnedAndOrdered(t *testing.T) {
	r := testrepo.GoApp(t)
	p, err := Make(r, Options{Base: "main", Head: "feature", MaxLines: 10_000, Rules: []Rule{
		{Name: "contracts", Paths: []string{"internal/types/"}},
		{Name: "docs", Paths: []string{"*.md"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := names(p)
	// contracts goes before everything that imports it; the remaining code
	// packs into one slice; the docs rule stays its own slice.
	if got[0] != "deps" && got[0] != "contracts" {
		t.Fatalf("unexpected first slice in %v", got)
	}
	var ci, di int
	for i, n := range got {
		switch n {
		case "contracts":
			ci = i
		case "docs":
			di = i
		}
	}
	if ci >= di {
		t.Fatalf("rule order not kept: %v", got)
	}
	for _, s := range p.Slices {
		if s.Name == "docs" && (len(s.Files) != 1 || s.Files[0].Path != "docs/guide.md") {
			t.Fatalf("docs slice = %+v", s.Files)
		}
	}
}

func TestPlanTypeScriptRelativeImports(t *testing.T) {
	r := testrepo.New(t)
	testrepo.Commit(t, r, "base", map[string]string{"package.json": "{}\n"})
	testrepo.Must(t, r, "checkout", "-q", "-b", "feature")
	testrepo.Commit(t, r, "ts", map[string]string{
		"src/app/index.ts": "import { add } from '../util/math'\nexport const x = add(1, 2)\n",
		"src/util/math.ts": "export function add(a: number, b: number) { return a + b }\n",
	})
	p, err := Make(r, Options{Base: "main", Head: "feature", MaxLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	// alphabetically src/app sorts first; the import edge must win
	if got := names(p); !reflect.DeepEqual(got, []string{"src/util", "src/app"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestPlanCycleStaysTogether(t *testing.T) {
	r := testrepo.New(t)
	testrepo.Commit(t, r, "base", map[string]string{"README.md": "x\n"})
	testrepo.Must(t, r, "checkout", "-q", "-b", "feature")
	testrepo.Commit(t, r, "cycle", map[string]string{
		"a/a.ts": "import { b } from '../b/b'\nexport const a = 1\n",
		"b/b.ts": "import { a } from '../a/a'\nexport const b = 2\n",
		"c/c.ts": "export const c = 3\n",
	})
	p, err := Make(r, Options{Base: "main", Head: "feature", MaxLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Slices) != 2 {
		t.Fatalf("want the a<->b cycle merged plus c, got %v", names(p))
	}
	if len(p.Warnings) == 0 || !strings.Contains(p.Warnings[0], "cycle") {
		t.Fatalf("expected a cycle warning, got %v", p.Warnings)
	}
}

func sliceOf(p *Plan, file string, status byte) int {
	for i, s := range p.Slices {
		for _, f := range s.Files {
			if f.Path == file && f.Status == status {
				return i
			}
		}
	}
	return -1
}

// A package moved to another directory is planned as add, switch importers,
// delete: no cycle, and every step has both copies or only the one in use.
func TestPlanMoveAcrossDirsOrdersAddImportersDelete(t *testing.T) {
	r := testrepo.New(t)
	testrepo.Commit(t, r, "base", map[string]string{
		"go.mod":             "module example.com/m\n\ngo 1.22\n",
		"oldpkg/x.go":        "package oldpkg\n\n// Value is shared by many callers in the code base.\nfunc Value() int { return 42 }\n",
		"caller/caller.go":   "package caller\n\nimport \"example.com/m/oldpkg\"\n\nfunc V() int { return oldpkg.Value() }\n",
		"other/other.go":     "package other\n\nimport \"example.com/m/oldpkg\"\n\nfunc W() int { return oldpkg.Value() }\n",
		"unrelated/other.go": "package unrelated\n",
	})
	testrepo.Must(t, r, "checkout", "-q", "-b", "feature")
	testrepo.Must(t, r, "mv", "oldpkg", "newpkg")
	testrepo.Commit(t, r, "move", map[string]string{
		"newpkg/x.go":        "package newpkg\n\n// Value is shared by many callers in the code base.\nfunc Value() int { return 42 }\n",
		"caller/caller.go":   "package caller\n\nimport \"example.com/m/newpkg\"\n\nfunc V() int { return newpkg.Value() }\n",
		"other/other.go":     "package other\n\nimport \"example.com/m/newpkg\"\n\nfunc W() int { return newpkg.Value() }\n",
		"unrelated/other.go": "package unrelated\n\nconst X = 1\n",
	})
	p, err := Make(r, Options{Base: "main", Head: "feature", MaxLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range p.Warnings {
		if strings.Contains(w, "cycle") {
			t.Fatalf("a move must not create a cycle: %v", p.Warnings)
		}
	}
	add := sliceOf(p, "newpkg/x.go", 'A')
	del := sliceOf(p, "oldpkg/x.go", 'D')
	caller := sliceOf(p, "caller/caller.go", 'M')
	other := sliceOf(p, "other/other.go", 'M')
	if add < 0 || del < 0 || caller < 0 || other < 0 {
		t.Fatalf("missing changes (add %d, del %d, caller %d, other %d): %v", add, del, caller, other, names(p))
	}
	if !(add < caller && add < other && caller < del && other < del) {
		t.Fatalf("want new copy, then importers, then delete; got add %d, caller %d, other %d, delete %d", add, caller, other, del)
	}
}

// A rename inside one directory keeps both halves together: two files in one
// package would otherwise declare the same symbols.
func TestPlanRenameInSameDirStaysWhole(t *testing.T) {
	r := testrepo.New(t)
	body := "package p\n\n// Value is shared by many callers in the code base.\nfunc Value() int { return 42 }\n"
	testrepo.Commit(t, r, "base", map[string]string{
		"go.mod":   "module example.com/m\n\ngo 1.22\n",
		"p/old.go": body,
	})
	testrepo.Must(t, r, "checkout", "-q", "-b", "feature")
	testrepo.Must(t, r, "mv", "p/old.go", "p/new.go")
	testrepo.Commit(t, r, "rename", map[string]string{"p/new.go": body})
	p, err := Make(r, Options{Base: "main", Head: "feature", MaxLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := sliceOf(p, "p/new.go", 'R'); got < 0 {
		t.Fatalf("a same-directory rename should stay one R change: %v", names(p))
	}
	if sliceOf(p, "p/old.go", 'D') >= 0 {
		t.Fatalf("old path must not be planned as a separate delete: %v", names(p))
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"*.md", "docs/a.md", true},
		{"*.md", "a.md", true},
		{"docs/", "docs/a/b.txt", true},
		{"docs/", "x/docs/a", true}, // gitignore semantics: any depth
		{"/docs/", "x/docs/a", false},
		{"/docs/", "docs/a", true},
		{"proto", "api/proto/x.proto", true},
		{"internal/**/types.go", "internal/a/b/types.go", true},
		{"internal/**/types.go", "internal/types.go", true},
		{"/cmd/*", "cmd/app/main.go", true},
		{"/cmd/*", "x/cmd/app", false},
		{"*.go", "main.ts", false},
	}
	for _, c := range cases {
		re, err := compileGlob(c.pat)
		if err != nil {
			t.Fatal(err)
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("%q vs %q = %v, want %v (re %s)", c.pat, c.path, got, c.want, re)
		}
	}
}

var shopFlags = &Flags{
	Paths:       []string{"internal/flags/"},
	Activation:  []string{"config/prod.yaml"},
	Guards:      []string{"flags.Enabled"},
	Entrypoints: []string{"HandleFunc"},
}

func TestPlanFlagsFirstActivationHeld(t *testing.T) {
	r := testrepo.FlagApp(t, "if")
	// A huge budget would pack everything together; flags and activation
	// still get slices of their own.
	p, err := Make(r, Options{Base: "main", Head: "feature", MaxLines: 10_000, Flags: shopFlags})
	if err != nil {
		t.Fatal(err)
	}
	got := names(p)
	if len(got) != 3 || got[0] != "flags" || got[2] != "activation" {
		t.Fatalf("want flags, code, activation; got %v", got)
	}
	if !p.Slices[2].Hold || p.Slices[0].Hold || p.Slices[1].Hold {
		t.Fatal("only the activation slice may be held")
	}
	if len(p.Findings) != 0 {
		t.Fatalf("guarded route reported: %+v", p.Findings)
	}
}

func TestGuardLint(t *testing.T) {
	for _, c := range []struct {
		guard string
		want  int
	}{{"if", 0}, {"return", 0}, {"none", 1}} {
		r := testrepo.FlagApp(t, c.guard)
		p, err := Make(r, Options{Base: "main", Head: "feature", MaxLines: 1, Flags: shopFlags})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Findings) != c.want {
			t.Fatalf("%s: want %d findings, got %+v", c.guard, c.want, p.Findings)
		}
		if c.want == 1 {
			f := p.Findings[0]
			if f.Path != "cmd/shop/main.go" || f.Call != "mux.HandleFunc" {
				t.Fatalf("wrong finding %+v", f)
			}
			strict := *shopFlags
			strict.Enforce = true
			if _, err := Make(r, Options{Base: "main", Head: "feature", MaxLines: 1, Flags: &strict}); err == nil ||
				!strings.Contains(err.Error(), "not behind a flag check") {
				t.Fatalf("enforce should fail the plan, got %v", err)
			}
		}
	}
}
