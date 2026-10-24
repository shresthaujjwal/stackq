// Package split turns the diff of one large branch into an ordered list of
// slices that can each be reviewed, built and merged on their own.
//
// Files are grouped by directory (a Go package, a TS folder), groups are
// ordered so that anything a group imports lands in an earlier or the same
// slice, and consecutive groups are packed into slices up to a line budget.
package split

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"stackq/internal/git"
)

// Rule pins files matching Paths into their own slice, in rule order.
type Rule struct {
	Name  string   `json:"name"`
	Paths []string `json:"paths"`
}

type Options struct {
	Base     string // trunk ref, e.g. origin/main
	Head     string // the big branch
	MaxLines int    // per-slice budget (added+deleted lines); default 400
	Rules    []Rule
	Flags    *Flags
}

// Flags describes the repo's feature-flag layout so the split can ship
// flag definitions first and hold back whatever switches a feature on.
type Flags struct {
	// Paths are flag definitions (defaulting to off). They get their own
	// slice, right after dependency manifests and before any code.
	Paths []string `json:"paths"`
	// Activation are files that turn features on (env configs, rollout
	// percentages). They go into a final held slice that never auto-lands.
	Activation []string `json:"activation"`
	// Guards are flag-check calls, e.g. "flags.Enabled".
	Guards []string `json:"guards"`
	// Entrypoints are calls that expose code to traffic, e.g. "HandleFunc",
	// "RegisterWorkflow". Added calls must sit behind a guard.
	Entrypoints []string `json:"entrypoints"`
	// Enforce turns guard findings into errors.
	Enforce bool `json:"enforce"`
}

type Slice struct {
	Name   string
	Groups []string
	Files  []git.Change
	Lines  int
	Pinned bool // came from a rule or the flag layout
	// Hold marks the activation slice: it gets a draft PR that is restacked
	// onto trunk after the stack lands but never queued automatically.
	Hold bool
}

// Finding is an entrypoint added outside any flag guard.
type Finding struct {
	Slice int // 0-based
	Path  string
	Line  int
	Call  string
}

type Plan struct {
	Base     string // merge-base SHA of trunk and head
	Head     string // head SHA
	Slices   []Slice
	Warnings []string
	Findings []Finding
}

type group struct {
	key   string
	name  string
	files []git.Change
	lines int
	cat   int
	rule  int    // index of the rule that produced it, or -1
	pin   string // "flags" or "activation"
}

type graph struct {
	groups map[string]*group
	deps   map[string]map[string]bool // a -> groups that must land before a
}

func (g *graph) edge(a, b string) {
	if a == b || a == "" || b == "" {
		return
	}
	if g.deps[a] == nil {
		g.deps[a] = map[string]bool{}
	}
	g.deps[a][b] = true
}

func isManifest(p string) bool {
	switch path.Base(p) {
	case "go.mod", "go.sum", "go.work", "go.work.sum", "package.json", "package-lock.json",
		"pnpm-lock.yaml", "pnpm-workspace.yaml", "yarn.lock", "bun.lock", "bun.lockb":
		return true
	}
	return false
}

// category is only a tie-breaker between groups with no dependency between
// them: contracts first, then libraries, entrypoints, infra, docs.
func category(p string) int {
	lp := strings.ToLower(p)
	base, ext := path.Base(lp), path.Ext(lp)
	has := func(seg string) bool { return strings.HasPrefix(lp, seg+"/") || strings.Contains(lp, "/"+seg+"/") }
	switch {
	case ext == ".proto" || ext == ".sql" || ext == ".graphql" || ext == ".gql" ||
		has("proto") || has("schema") || has("schemas") || has("migrations") || has("types"):
		return 0
	case ext == ".md" || ext == ".mdx" || has("docs"):
		return 4
	case has(".github") || base == "dockerfile" || base == "makefile" || has("deploy") || has("infra") ||
		has("test") || has("tests") || has("e2e"):
		return 3
	case has("cmd") || has("app") || has("apps") || has("server") || base == "main.go":
		return 2
	default:
		return 1
	}
}

type compiledRule struct {
	name string
	res  []*regexp.Regexp
}

func compileRules(rules []Rule) ([]compiledRule, error) {
	var out []compiledRule
	for _, r := range rules {
		cr := compiledRule{name: r.Name}
		for _, p := range r.Paths {
			re, err := compileGlob(p)
			if err != nil {
				return nil, fmt.Errorf("rule %q: bad pattern %q: %w", r.Name, p, err)
			}
			cr.res = append(cr.res, re)
		}
		out = append(out, cr)
	}
	return out, nil
}

func compileAll(pats []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, p := range pats {
		re, err := compileGlob(p)
		if err != nil {
			return nil, fmt.Errorf("bad pattern %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func matchAny(res []*regexp.Regexp, p string) bool {
	for _, re := range res {
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

func matchRule(rules []compiledRule, p string) int {
	for i, r := range rules {
		for _, re := range r.res {
			if re.MatchString(p) {
				return i
			}
		}
	}
	return -1
}

// Make computes a split plan without touching any refs.
func Make(r *git.Repo, o Options) (*Plan, error) {
	if o.MaxLines <= 0 {
		o.MaxLines = 400
	}
	base, err := r.MergeBase(o.Base, o.Head)
	if err != nil {
		return nil, err
	}
	head, err := r.RevParse(o.Head)
	if err != nil {
		return nil, err
	}
	changes, err := r.Diff(base, head)
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return nil, fmt.Errorf("%s has no changes relative to %s", o.Head, o.Base)
	}
	rules, err := compileRules(o.Rules)
	if err != nil {
		return nil, err
	}
	var flagRes, actRes []*regexp.Regexp
	if o.Flags != nil {
		if flagRes, err = compileAll(o.Flags.Paths); err != nil {
			return nil, fmt.Errorf("flags.paths: %w", err)
		}
		if actRes, err = compileAll(o.Flags.Activation); err != nil {
			return nil, fmt.Errorf("flags.activation: %w", err)
		}
	}

	plan := &Plan{Base: base, Head: head}
	g := &graph{groups: map[string]*group{}, deps: map[string]map[string]bool{}}
	fileKey := map[string]string{}
	dirIndex := map[string][]string{}  // dir -> groups with files there (at head)
	deletedIn := map[string][]string{} // dir -> groups that remove files from it

	addIdx := func(m map[string][]string, dir, key string) {
		for _, k := range m[dir] {
			if k == key {
				return
			}
		}
		m[dir] = append(m[dir], key)
	}

	for _, c := range changes {
		key, name, rule, pin := "", "", -1, ""
		switch i := matchRule(rules, c.Path); {
		case matchAny(actRes, c.Path):
			key, name, pin = "activation", "activation", "activation"
		case matchAny(flagRes, c.Path):
			key, name, pin = "flags", "flags", "flags"
		case i >= 0:
			key, name, rule = fmt.Sprintf("rule:%03d", i), rules[i].name, i
		case isManifest(c.Path):
			key, name = "deps", "deps"
		default:
			key, name = "dir:"+path.Dir(c.Path), path.Dir(c.Path)
		}
		gr := g.groups[key]
		if gr == nil {
			gr = &group{key: key, name: name, cat: 99, rule: rule, pin: pin}
			g.groups[key] = gr
		}
		gr.files = append(gr.files, c)
		gr.lines += c.Lines()
		if cat := category(c.Path); cat < gr.cat {
			gr.cat = cat
		}
		if key == "deps" || key == "flags" {
			gr.cat = -1
		}
		fileKey[c.Path] = key
		if c.Status != 'D' {
			addIdx(dirIndex, path.Dir(c.Path), key)
		}
		if c.Status == 'D' {
			addIdx(deletedIn, path.Dir(c.Path), key)
		}
		if c.Status == 'R' {
			addIdx(deletedIn, path.Dir(c.OldPath), key)
		}
	}

	mods := loadGoModules(r, head)
	baseMods := loadGoModules(r, base)

	resolve := func(target string) []string {
		if ks, ok := dirIndex[target]; ok {
			return ks
		}
		return dirIndex[path.Dir(target)]
	}

	for _, c := range changes {
		own := fileKey[c.Path]
		// What this file imports at head must already exist when it lands.
		if c.Status != 'D' {
			for _, t := range importTargets(r, head, c.Path, mods) {
				for _, k := range resolve(t) {
					g.edge(own, k)
				}
			}
		}
		// Anything this file used to import must not be deleted before
		// this file stops importing it.
		if c.Status != 'A' {
			oldPath := c.Path
			if c.OldPath != "" {
				oldPath = c.OldPath
			}
			for _, t := range importTargets(r, base, oldPath, baseMods) {
				cands := []string{t, path.Dir(t)} // JS: a folder or a file in one
				if isGo(oldPath) {
					cands = cands[:1] // Go imports name the package directory
				}
				for _, d := range cands {
					for _, k := range deletedIn[d] {
						g.edge(k, own)
					}
				}
			}
		}
	}

	// Dependency manifests go first; pinned rules keep their order.
	var ruleKeys []string
	for k, gr := range g.groups {
		if k != "deps" && g.groups["deps"] != nil {
			g.edge(k, "deps")
		}
		if gr.rule >= 0 {
			ruleKeys = append(ruleKeys, k)
		}
	}
	sort.Strings(ruleKeys)
	for i := 1; i < len(ruleKeys); i++ {
		g.edge(ruleKeys[i], ruleKeys[i-1])
	}
	// Flag definitions go before any code; activation after everything.
	for k := range g.groups {
		if _, ok := g.groups["flags"]; ok && k != "flags" && k != "deps" {
			g.edge(k, "flags")
		}
		if _, ok := g.groups["activation"]; ok && k != "activation" {
			g.edge("activation", k)
		}
	}

	ordered := g.order(plan)

	// Pack consecutive groups into slices.
	var cur *Slice
	flush := func() {
		if cur != nil {
			plan.Slices = append(plan.Slices, *cur)
			cur = nil
		}
	}
	for _, gr := range ordered {
		pinned := gr.rule >= 0 || gr.pin != ""
		if cur != nil && (pinned || cur.Pinned || cur.Lines+gr.lines > o.MaxLines) {
			flush()
		}
		if cur == nil {
			cur = &Slice{Pinned: pinned, Hold: gr.pin == "activation"}
			if pinned {
				cur.Name = gr.name
			}
		}
		cur.Groups = append(cur.Groups, gr.name)
		cur.Files = append(cur.Files, gr.files...)
		cur.Lines += gr.lines
		if gr.lines > o.MaxLines {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"%s alone is %d lines (budget %d); add a rule or split it by hand if reviewers need it smaller",
				gr.name, gr.lines, o.MaxLines))
		}
	}
	flush()

	for i := range plan.Slices {
		s := &plan.Slices[i]
		sort.Slice(s.Files, func(a, b int) bool { return s.Files[a].Path < s.Files[b].Path })
		if s.Name == "" {
			s.Name = nameFor(s.Groups)
		}
	}
	if o.Flags != nil {
		if g.groups["flags"] == nil && len(o.Flags.Paths) > 0 {
			plan.Warnings = append(plan.Warnings, "no flag definitions changed; make sure the new code is behind an existing flag")
		}
		if err := checkGuards(r, plan, o.Flags); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// order collapses import cycles into single groups and returns groups in a
// dependency-respecting order, breaking ties by category then name.
func (g *graph) order(plan *Plan) []*group {
	keys := make([]string, 0, len(g.groups))
	for k := range g.groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	comps := g.sccs(keys)
	compOf := map[string]int{}
	merged := make([]*group, len(comps))
	for i, comp := range comps {
		m := &group{key: comp[0], cat: 99, rule: -1}
		var names []string
		for _, k := range comp {
			gr := g.groups[k]
			compOf[k] = i
			m.files = append(m.files, gr.files...)
			m.lines += gr.lines
			m.cat = min(m.cat, gr.cat)
			if gr.rule >= 0 && (m.rule < 0 || gr.rule < m.rule) {
				m.rule = gr.rule
			}
			if gr.pin != "" && m.pin != "activation" {
				m.pin = gr.pin
			}
			names = append(names, gr.name)
		}
		m.name = names[0]
		if m.rule >= 0 {
			for _, k := range comp {
				if g.groups[k].rule == m.rule {
					m.name = g.groups[k].name
				}
			}
		}
		if len(comp) > 1 {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"import cycle kept together in one slice: %s", strings.Join(names, ", ")))
			if m.pin != "" {
				m.name = m.pin
				plan.Warnings = append(plan.Warnings, fmt.Sprintf(
					"the %s slice depends on %s, so they ship together; consider moving that code out", m.pin, strings.Join(names, ", ")))
			} else if m.rule < 0 {
				m.name = nameFor(names)
			}
		}
		merged[i] = m
	}

	before := make([]map[int]bool, len(merged))
	for i := range before {
		before[i] = map[int]bool{}
	}
	for a, bs := range g.deps {
		for b := range bs {
			if ca, cb := compOf[a], compOf[b]; ca != cb {
				before[ca][cb] = true
			}
		}
	}

	less := func(a, b *group) bool {
		if a.cat != b.cat {
			return a.cat < b.cat
		}
		return a.key < b.key
	}
	done := make([]bool, len(merged))
	var out []*group
	for len(out) < len(merged) {
		best := -1
		for i, m := range merged {
			if done[i] {
				continue
			}
			ready := true
			for b := range before[i] {
				if !done[b] {
					ready = false
					break
				}
			}
			if ready && (best < 0 || less(m, merged[best])) {
				best = i
			}
		}
		done[best] = true // the condensation is a DAG, so best is always found
		out = append(out, merged[best])
	}
	return out
}

// sccs returns strongly connected components (Tarjan).
func (g *graph) sccs(keys []string) [][]string {
	index := 0
	idx, low := map[string]int{}, map[string]int{}
	on := map[string]bool{}
	var stack []string
	var out [][]string
	var visit func(v string)
	visit = func(v string) {
		idx[v], low[v] = index, index
		index++
		stack = append(stack, v)
		on[v] = true
		var ws []string
		for w := range g.deps[v] {
			ws = append(ws, w)
		}
		sort.Strings(ws)
		for _, w := range ws {
			if _, seen := idx[w]; !seen {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if on[w] {
				low[v] = min(low[v], idx[w])
			}
		}
		if low[v] == idx[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				on[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			sort.Strings(comp)
			out = append(out, comp)
		}
	}
	for _, k := range keys {
		if _, ok := idx[k]; !ok {
			visit(k)
		}
	}
	return out
}

func nameFor(groups []string) string {
	var dirs []string
	for _, g := range groups {
		if g != "deps" {
			dirs = append(dirs, g)
		}
	}
	if len(dirs) == 0 {
		return "deps"
	}
	if len(dirs) == 1 {
		return dirs[0]
	}
	common := strings.Split(dirs[0], "/")
	for _, d := range dirs[1:] {
		parts := strings.Split(d, "/")
		n := 0
		for n < len(common) && n < len(parts) && common[n] == parts[n] {
			n++
		}
		common = common[:n]
	}
	if c := strings.Join(common, "/"); c != "" && c != "." {
		return c
	}
	return fmt.Sprintf("%s-and-%d-more", dirs[0], len(dirs)-1)
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug makes a branch-safe name fragment.
func Slug(s string) string {
	if s == "." {
		return "root"
	}
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		return "slice"
	}
	return s
}
