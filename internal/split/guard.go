package split

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// checkGuards flags entrypoint calls added outside a flag guard, so a
// slice can't expose half-built code to traffic. Go only; it is a lint,
// not a proof (see SPEC §29.3).
func checkGuards(r interface {
	Show(rev, path string) ([]byte, error)
	AddedLines(base, head, path string) (map[int]bool, error)
}, plan *Plan, f *Flags) error {
	if len(f.Entrypoints) == 0 || len(f.Guards) == 0 {
		return nil
	}
	for si, sl := range plan.Slices {
		if sl.Hold || sl.Name == "flags" {
			continue
		}
		for _, c := range sl.Files {
			if !isGo(c.Path) || c.Status == 'D' || strings.HasSuffix(c.Path, "_test.go") {
				continue
			}
			src, err := r.Show(plan.Head, c.Path)
			if err != nil {
				continue
			}
			added, err := r.AddedLines(plan.Base, plan.Head, c.Path)
			if err != nil || len(added) == 0 {
				continue
			}
			for _, u := range unguardedCalls(src, f.Entrypoints, f.Guards) {
				if added[u.line] {
					plan.Findings = append(plan.Findings, Finding{Slice: si, Path: c.Path, Line: u.line, Call: u.call})
				}
			}
		}
	}
	if f.Enforce && len(plan.Findings) > 0 {
		var b strings.Builder
		for _, x := range plan.Findings {
			fmt.Fprintf(&b, "\n  slice %d %s:%d %s() is not behind a flag check (%s)",
				x.Slice+1, x.Path, x.Line, x.Call, strings.Join(f.Guards, " or "))
		}
		return fmt.Errorf("flag guard check failed:%s", b.String())
	}
	return nil
}

type unguarded struct {
	line int
	call string
}

// calleeName renders a call target like "mux.HandleFunc" or "w.RegisterWorkflow".
func calleeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if x := calleeName(v.X); x != "" {
			return x + "." + v.Sel.Name
		}
		return v.Sel.Name
	case *ast.IndexExpr: // generic instantiation
		return calleeName(v.X)
	case *ast.CallExpr: // chained: a.B().C
		return calleeName(v.Fun) + "()"
	}
	return ""
}

// nameMatches: "HandleFunc" matches any x.HandleFunc; "flags.Enabled"
// matches flags.Enabled and cfg.flags.Enabled.
func nameMatches(callee string, pats []string) bool {
	for _, p := range pats {
		if callee == p || strings.HasSuffix(callee, "."+p) {
			return true
		}
	}
	return false
}

func hasGuard(n ast.Node, guards []string) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if c, ok := x.(*ast.CallExpr); ok && nameMatches(calleeName(c.Fun), guards) {
			found = true
		}
		return !found
	})
	return found
}

// earlyReturnGuard matches `if !flags.Enabled(x) { return }`.
func earlyReturnGuard(s ast.Stmt, guards []string) bool {
	is, ok := s.(*ast.IfStmt)
	if !ok || is.Else != nil || len(is.Body.List) == 0 || !hasGuard(is.Cond, guards) {
		return false
	}
	_, ret := is.Body.List[len(is.Body.List)-1].(*ast.ReturnStmt)
	return ret
}

func unguardedCalls(src []byte, entrypoints, guards []string) []unguarded {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var out []unguarded
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := calleeName(call.Fun)
		if !nameMatches(name, entrypoints) || nameMatches(name, guards) {
			return true
		}
		if !guardedBy(stack, guards) {
			out = append(out, unguarded{line: fset.Position(call.Pos()).Line, call: name})
		}
		return true
	})
	return out
}

// guardedBy walks the ancestors of the innermost node in stack.
func guardedBy(stack []ast.Node, guards []string) bool {
	for i := len(stack) - 2; i >= 0; i-- {
		child := stack[i+1]
		switch a := stack[i].(type) {
		case *ast.IfStmt:
			if child == ast.Node(a.Body) && hasGuard(a.Cond, guards) {
				return true
			}
		case *ast.BlockStmt:
			for _, st := range a.List {
				if ast.Node(st) == child {
					break
				}
				if earlyReturnGuard(st, guards) {
					return true
				}
			}
		case *ast.FuncDecl, *ast.FuncLit:
			// Guards don't cross function boundaries: a guarded caller
			// of this function is out of scope for a lint.
			return false
		}
	}
	return false
}
