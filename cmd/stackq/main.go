// Command stackq splits a large branch into a stack of small, ordered PRs
// and lands them through GitHub's merge queue.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"stackq/internal/doctor"
	"stackq/internal/forge"
	"stackq/internal/git"
	"stackq/internal/land"
	"stackq/internal/prio"
	"stackq/internal/split"
	"stackq/internal/stack"
)

// Set at release time: -X main.version=v1.2.3 -X main.commit=abc1234.
var (
	version = "dev"
	commit  = ""
)

func versionString() string {
	if commit == "" {
		return "stackq " + version
	}
	return "stackq " + version + " (" + commit + ")"
}

const usage = `stackq: split a big branch into stacked PRs and land them through the merge queue

Local workflow:
  stackq plan  [--from BRANCH] [--max-lines N]       preview the split
  stackq split [--from BRANCH] [--check CMD]         create the stack branches
  stackq push  [--draft] [--close-source]            push branches, open/retarget PRs,
                                                     link (and optionally close) the original PR
  stackq land  [--pr N]                              atomic: gate every part, queue the whole stack once
                                                     incremental: queue the bottom PR
  stackq sync  [--no-fetch] [--trunk]                restack after edits or merges
  stackq revert --pr N [--only] [--enqueue]          open one PR that reverts a landed stack
  stackq queue --pr N                                add any PR to the priority pool, then schedule
  stackq schedule [--dry-run]                        rank the pool and top up the merge queue
  stackq status | list
  stackq doctor                                      check the repository is set up for stackq (read-only)
  stackq gc [--delete]                               list (or delete) branches left behind by merged or finished stacks

CI (GitHub Actions):
  stackq ci                                          act on the current event
  stackq ci after-merge|dequeued|land|revert --pr N  or run a step directly
  stackq ci schedule                                 one priority pass
  stackq ci doctor                                   the setup check, with the workflow's own token
  stackq ci gc                                       delete branches left behind (gc --delete)

  stackq version                                     print the release this binary was built from

Common flags: -C DIR, --stack NAME, --remote NAME
Config: .stackq.json at the repo root (trunk, remote, prefix, maxLines, check,
landing, minApprovals, flags, priority, slices).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "plan":
		err = cmdSplit(args, true)
	case "split":
		err = cmdSplit(args, false)
	case "status":
		err = cmdStatus(args)
	case "list":
		err = cmdList(args)
	case "sync":
		err = cmdSync(args)
	case "push":
		err = cmdPush(args)
	case "land":
		err = cmdLand(args)
	case "revert":
		err = cmdRevert(args)
	case "schedule":
		err = cmdSchedule(args)
	case "queue":
		err = cmdQueue(args)
	case "ci":
		err = cmdCI(args)
	case "doctor":
		err = cmdDoctor(args)
	case "gc":
		err = cmdGC(args)
	case "version", "-v", "--version":
		fmt.Println(versionString())
		return
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "stackq:", err)
		}
		os.Exit(1)
	}
}

type config struct {
	Trunk       string       `json:"trunk"`
	Remote      string       `json:"remote"`
	Prefix      string       `json:"prefix"`
	MaxLines    int          `json:"maxLines"`
	Check       string       `json:"check"`
	MergeOnFail *bool        `json:"mergeOnFail"`
	CloseSource bool         `json:"closeSource"`
	Slices      []split.Rule `json:"slices"`
	// Landing is "atomic" (default) or "incremental".
	Landing      string `json:"landing"`
	MinApprovals int    `json:"minApprovals"`
	KeepBranches bool   `json:"keepBranches"`
	// AutoLand lands an atomic stack by itself once every part is approved and
	// green (checked on reviews, finished CI runs and the timer).
	AutoLand bool `json:"autoLand"`
	// AllowUnreviewed skips the approval gate for atomic landings (CI and
	// land). Checks, requested changes and stacking are still enforced.
	AllowUnreviewed bool         `json:"allowUnreviewed"`
	Flags           *split.Flags `json:"flags"`
	Priority        *prio.Config `json:"priority"`
}

func (c *common) runner(r *git.Repo) *land.Runner {
	return &land.Runner{Repo: r, Forge: &forge.GH{Dir: r.Dir, Log: os.Stdout}, Remote: c.remote, Log: os.Stdout,
		Prefix: c.cfg.Prefix, MinApprovals: c.cfg.MinApprovals, AllowUnreviewed: c.cfg.AllowUnreviewed, KeepBranches: c.cfg.KeepBranches, Prio: c.cfg.Priority}
}

type common struct {
	dir, stack, remote string
	cfg                config
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.dir, "C", ".", "repository directory")
	fs.StringVar(&c.stack, "stack", "", "stack name (default: the stack holding the current branch)")
	fs.StringVar(&c.remote, "remote", "", "git remote (default: config, then origin if it exists)")
}

func (c *common) repo() (*git.Repo, error) {
	r := &git.Repo{Dir: c.dir}
	top, err := r.TopLevel()
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git repository", c.dir)
	}
	r.Dir = top
	if data, err := os.ReadFile(filepath.Join(top, ".stackq.json")); err == nil {
		if err := json.Unmarshal(data, &c.cfg); err != nil {
			return nil, fmt.Errorf(".stackq.json: %w", err)
		}
	}
	if c.cfg.Priority != nil {
		pc := c.cfg.Priority.WithDefaults()
		if err := pc.Validate(); err != nil {
			return nil, fmt.Errorf(".stackq.json: %w", err)
		}
		c.cfg.Priority = &pc
	}
	if c.remote == "" {
		c.remote = c.cfg.Remote
	}
	if c.remote == "" && r.HasRemote("origin") {
		c.remote = "origin"
	}
	return r, nil
}

func (c *common) trunk(r *git.Repo, flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if c.cfg.Trunk != "" {
		return c.cfg.Trunk
	}
	if c.remote != "" {
		if out, err := r.Run("symbolic-ref", "--short", "refs/remotes/"+c.remote+"/HEAD"); err == nil {
			return strings.TrimPrefix(out, c.remote+"/")
		}
	}
	if !r.RefExists("main") && r.RefExists("master") {
		return "master"
	}
	return "main"
}

func parse(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(os.Stderr)
	return fs.Parse(args)
}

func cmdSplit(args []string, dry bool) error {
	fs := flag.NewFlagSet("split", flag.ContinueOnError)
	var c common
	c.register(fs)
	from := fs.String("from", "", "branch to split (default: current branch)")
	trunkF := fs.String("trunk", "", "trunk branch the stack lands on (default: main)")
	name := fs.String("name", "", "stack name (default: derived from --from)")
	prefix := fs.String("prefix", "", "branch prefix (default: stack/<name>/)")
	maxLines := fs.Int("max-lines", 0, "changed-line budget per slice (default 400)")
	check := fs.String("check", "", `command every slice must pass, e.g. "go build ./..."`)
	noFold := fs.Bool("no-fold", false, "fail instead of folding a slice that fails --check into the next one")
	force := fs.Bool("force", false, "replace existing branches/stack of the same name")
	fromPR := fs.Int("from-pr", 0, "the original PR (default: found on push from the source branch)")
	landing := fs.String("landing", "", `"atomic" (whole stack lands as one queue entry; default) or "incremental"`)
	if err := parse(fs, args); err != nil {
		return err
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	if *landing == "" {
		*landing = c.cfg.Landing
	}
	switch *landing {
	case "", stack.Atomic:
		*landing = stack.Atomic
	case "incremental":
	default:
		return fmt.Errorf("--landing must be atomic or incremental")
	}
	src := *from
	if src == "" {
		if src = r.CurrentBranch(); src == "" {
			return fmt.Errorf("HEAD is detached; pass --from")
		}
	}
	trunk := c.trunk(r, *trunkF)
	if src == trunk {
		return fmt.Errorf("you are on %s; check out the big branch or pass --from", trunk)
	}
	if *name == "" {
		*name = split.Slug(path.Base(src))
	}
	if *prefix == "" {
		*prefix = c.cfg.Prefix
		if *prefix == "" {
			*prefix = "stack/" + *name + "/"
		} else {
			*prefix = strings.ReplaceAll(*prefix, "{name}", *name)
		}
	}
	if !strings.HasSuffix(*prefix, "/") {
		*prefix += "/"
	}
	if *maxLines == 0 {
		*maxLines = c.cfg.MaxLines
	}
	if *check == "" {
		*check = c.cfg.Check
	}
	fold := !*noFold
	if c.cfg.MergeOnFail != nil && !*noFold {
		fold = *c.cfg.MergeOnFail
	}

	baseRef := trunk
	if c.remote != "" && r.RefExists("refs/remotes/"+c.remote+"/"+trunk) {
		baseRef = c.remote + "/" + trunk
	}
	plan, err := split.Make(r, split.Options{Base: baseRef, Head: src, MaxLines: *maxLines, Rules: c.cfg.Slices, Flags: c.cfg.Flags})
	if err != nil {
		return err
	}
	printPlan(src, baseRef, plan)
	if dry {
		fmt.Println("\nDry run. Create the branches with: stackq split " + strings.Join(args, " "))
		return nil
	}
	fmt.Println()
	s, err := stack.Create(r, plan, stack.CreateOptions{
		Name: *name, Prefix: *prefix, Trunk: trunk, Remote: c.remote, Source: src,
		Check: *check, MergeOnFail: fold, Force: *force, SourcePR: *fromPR, Landing: *landing, Log: os.Stdout,
	})
	if err != nil {
		return err
	}
	fmt.Printf("\nCreated stack %q (%d branches, %s landing). The top branch matches %s exactly.\n", s.Name, len(s.Branches), *landing, src)
	for _, b := range s.Branches {
		fmt.Printf("  %s\n", b.Name)
	}
	fmt.Println("\nNext: stackq push   (then stackq land, or let CI take it from there)")
	return nil
}

func printPlan(src, base string, p *split.Plan) {
	fmt.Printf("Split %s into %d slices on %s (fork point %s):\n", src, len(p.Slices), base, git.Short(p.Base))
	for i, s := range p.Slices {
		add, del := 0, 0
		for _, f := range s.Files {
			add += f.Added
			del += f.Deleted
		}
		pin := ""
		switch {
		case s.Hold:
			pin = " [held: activation, merged by hand]"
		case s.Name == "flags" && s.Pinned:
			pin = " [flags first]"
		case s.Pinned:
			pin = " [rule]"
		}
		fmt.Printf("  %02d %-36s %3d files  +%d/-%d%s\n", i+1, s.Name, len(s.Files), add, del, pin)
		for j, f := range s.Files {
			if j == 6 {
				fmt.Printf("       … and %d more\n", len(s.Files)-6)
				break
			}
			desc := f.Path
			if f.Status == 'R' {
				desc = f.OldPath + " → " + f.Path
			}
			fmt.Printf("       %c %s\n", f.Status, desc)
		}
	}
	for _, w := range p.Warnings {
		fmt.Printf("  ! %s\n", w)
	}
	for _, f := range p.Findings {
		fmt.Printf("  ! slice %02d %s:%d adds %s() outside a flag check\n", f.Slice+1, f.Path, f.Line, f.Call)
	}
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	var c common
	c.register(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	names, err := stack.List(r)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Println("no stacks")
	}
	for _, n := range names {
		s, err := stack.Load(r, n)
		if err != nil {
			fmt.Printf("%s (unreadable: %v)\n", n, err)
			continue
		}
		fmt.Printf("%s  %d/%d landed  from %s\n", n, len(s.Branches)-len(s.Active()), len(s.Branches), s.Source)
	}
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var c common
	c.register(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	s, err := stack.Resolve(r, c.stack)
	if err != nil {
		return err
	}
	fmt.Printf("Stack %s → %s (from %s)\n", s.Name, s.TrunkRef(r), s.Source)
	for i, b := range s.Branches {
		pr := "  -  "
		if b.PR > 0 {
			pr = fmt.Sprintf("#%-4d", b.PR)
		}
		if b.Merged {
			fmt.Printf("  %02d ✅ %s %s (landed)\n", i+1, pr, b.Name)
			continue
		}
		parent := b.Parent
		if parent == s.Trunk {
			parent = s.TrunkRef(r)
		}
		n := r.CountCommits(parent, b.Name)
		state := ""
		if tip, err := r.RevParse(parent); err == nil && !r.IsAncestor(tip, b.Name) {
			state = "  needs sync"
		}
		if sha, err := r.RevParse(b.Name); err == nil && b.PushedSHA != "" && sha != b.PushedSHA {
			state += "  unpushed"
		}
		fmt.Printf("  %02d ⬜ %s %s (%d commits on %s)%s\n", i+1, pr, b.Name, n, b.Parent, state)
	}
	return nil
}

func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	var c common
	c.register(fs)
	noFetch := fs.Bool("no-fetch", false, "don't fetch the remote first")
	trunk := fs.Bool("trunk", false, "also rebase the bottom branch onto the latest trunk")
	if err := parse(fs, args); err != nil {
		return err
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	s, err := stack.Resolve(r, c.stack)
	if err != nil {
		return err
	}
	if err := stack.Sync(r, s, stack.SyncOptions{Fetch: !*noFetch, RebaseTrunk: *trunk, Log: os.Stdout}); err != nil {
		return err
	}
	fmt.Printf("Stack %s is in sync (%d of %d left to land).\n", s.Name, len(s.Active()), len(s.Branches))
	return nil
}

func cmdPush(args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	var c common
	c.register(fs)
	draft := fs.Bool("draft", false, "open new PRs as drafts")
	fromPR := fs.Int("from-pr", 0, "the original PR to link (default: the open PR of the source branch)")
	noSource := fs.Bool("no-source-pr", false, "don't look up, link or comment on the original PR")
	closeSource := fs.Bool("close-source", false, "close the original PR once the stack PRs exist")
	if err := parse(fs, args); err != nil {
		return err
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	s, err := stack.Resolve(r, c.stack)
	if err != nil {
		return err
	}
	return stack.Push(r, &forge.GH{Dir: r.Dir, Log: os.Stdout}, s, stack.PushOptions{
		Draft: *draft, SourcePR: *fromPR, NoSourcePR: *noSource,
		CloseSource: *closeSource || c.cfg.CloseSource, Log: os.Stdout,
	})
}

func cmdLand(args []string) error {
	fs := flag.NewFlagSet("land", flag.ContinueOnError)
	var c common
	c.register(fs)
	prN := fs.Int("pr", 0, "PR to land (default: the stack's bottom PR)")
	minApprovals := fs.Int("min-approvals", 0, "atomic: approvals each part needs (default: config, then 1)")
	unreviewed := fs.Bool("allow-unreviewed", false, "atomic: skip the per-part approval gate")
	if err := parse(fs, args); err != nil {
		return err
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	n := *prN
	if n == 0 {
		s, err := stack.Resolve(r, c.stack)
		if err != nil {
			return err
		}
		active := s.Active()
		if len(active) == 0 {
			return fmt.Errorf("stack %s has fully landed", s.Name)
		}
		b := active[0]
		if b.PR == 0 {
			return fmt.Errorf("%s has no PR yet; run stackq push", b.Name)
		}
		if b.Parent != s.Trunk && s.Landing != stack.Atomic {
			return fmt.Errorf("%s is still stacked on %s; run stackq sync && stackq push", b.Name, b.Parent)
		}
		n = b.PR
	}
	run := c.runner(r)
	if *minApprovals > 0 {
		run.MinApprovals = *minApprovals
	}
	run.AllowUnreviewed = run.AllowUnreviewed || *unreviewed
	return run.Land(n)
}

func cmdSchedule(args []string) error {
	fs := flag.NewFlagSet("schedule", flag.ContinueOnError)
	var c common
	c.register(fs)
	dry := fs.Bool("dry-run", false, "show the ranking and decisions without queueing anything")
	trunkF := fs.String("trunk", "", "queue branch (default: config trunk)")
	if err := parse(fs, args); err != nil {
		return err
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	_, err = c.runner(r).Schedule(c.trunk(r, *trunkF), *dry)
	return err
}

func cmdQueue(args []string) error {
	fs := flag.NewFlagSet("queue", flag.ContinueOnError)
	var c common
	c.register(fs)
	prN := fs.Int("pr", 0, "PR to add to the priority pool")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *prN == 0 {
		return fmt.Errorf("queue needs --pr")
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	run := c.runner(r)
	if run.Prio == nil || !run.Prio.Enabled {
		return fmt.Errorf("prioritization is off; set priority.enabled in .stackq.json")
	}
	pr, err := run.Forge.GetPR(*prN)
	if err != nil {
		return err
	}
	if err := run.Forge.AddLabels(*prN, run.Prio.Label); err != nil {
		return err
	}
	_, err = run.Schedule(pr.BaseRef, false)
	return err
}

func cmdRevert(args []string) error {
	fs := flag.NewFlagSet("revert", flag.ContinueOnError)
	var c common
	c.register(fs)
	prN := fs.Int("pr", 0, "any landed PR of the stack (atomic: the lander)")
	only := fs.Bool("only", false, "revert just this PR, not the whole stack")
	enqueue := fs.Bool("enqueue", false, "queue the revert PR right away")
	force := fs.Bool("force", false, "open the revert even if it touches files the stack didn't")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *prN == 0 {
		return fmt.Errorf("revert needs --pr")
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	_, err = c.runner(r).Revert(*prN, land.RevertOptions{Only: *only, Enqueue: *enqueue, Force: *force})
	return err
}

func cmdCI(args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("ci", flag.ContinueOnError)
	var c common
	c.register(fs)
	prN := fs.Int("pr", 0, "pull request number")
	noEnqueue := fs.Bool("no-enqueue", false, "restack but don't queue the next PR")
	revertEnqueue := fs.Bool("revert-enqueue", false, "revert: queue the revert PR")
	if err := parse(fs, args); err != nil {
		return err
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	if c.remote == "" {
		return fmt.Errorf("no git remote found")
	}
	n := *prN
	poolLabel := ""
	if c.cfg.Priority != nil && c.cfg.Priority.Enabled {
		poolLabel = c.cfg.Priority.Label
	}
	if sub == "" {
		if sub, n, err = fromEvent(poolLabel, c.cfg.AutoLand); err != nil {
			return err
		}
		if sub == "" {
			fmt.Println("stackq: nothing to do for this event")
			return nil
		}
	}
	if sub == "doctor" {
		return runDoctor(r, &c)
	}
	if sub == "gc" {
		_, err := c.runner(r).GC(c.trunk(r, ""), true)
		return err
	}
	if n == 0 && sub != "schedule" && sub != "tick" {
		return fmt.Errorf("ci %s needs --pr", sub)
	}
	if out, _ := r.Run("config", "user.email"); out == "" {
		r.Env = append(r.Env,
			"GIT_COMMITTER_NAME=github-actions[bot]",
			"GIT_COMMITTER_EMAIL=41898283+github-actions[bot]@users.noreply.github.com",
			"GIT_AUTHOR_NAME=github-actions[bot]",
			"GIT_AUTHOR_EMAIL=41898283+github-actions[bot]@users.noreply.github.com")
	}
	run := c.runner(r)
	run.Enqueue = !*noEnqueue
	trunk := c.trunk(r, "")
	// A merge or an ejection frees a queue slot: refill it.
	afterwards := func(err error) error {
		if run.Prio == nil || !run.Prio.Enabled {
			return err
		}
		if _, serr := run.Schedule(trunk, false); serr != nil && err == nil {
			err = serr
		}
		return err
	}
	switch sub {
	case "schedule":
		_, err := run.Schedule(trunk, false)
		return err
	case "tick":
		// Tidy stacks that merged without their cleanup running, land stacks
		// that became ready, then refill the queue by priority.
		err := run.FinishLanded()
		if c.cfg.AutoLand {
			err = errors.Join(err, run.AutoLand())
		}
		if run.Prio != nil && run.Prio.Enabled {
			if _, serr := run.Schedule(trunk, false); serr != nil {
				err = errors.Join(err, serr)
			}
		}
		return err
	case "after-merge":
		return afterwards(run.AfterMerge(n))
	case "dequeued":
		return afterwards(run.Dequeued(n))
	case "land":
		return run.Land(n)
	case "revert":
		_, err := run.Revert(n, land.RevertOptions{Enqueue: *revertEnqueue})
		return err
	}
	return fmt.Errorf("unknown ci command %q (after-merge, dequeued, land, revert, schedule, tick)", sub)
}

// fromEvent maps the GitHub Actions event to a ci command.
func fromEvent(poolLabel string, autoLand bool) (string, int, error) {
	name, p := os.Getenv("GITHUB_EVENT_NAME"), os.Getenv("GITHUB_EVENT_PATH")
	if name == "" || p == "" {
		return "", 0, fmt.Errorf("not running in GitHub Actions; use stackq ci <after-merge|dequeued|land|revert|schedule> --pr N")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", 0, err
	}
	var ev struct {
		Action      string `json:"action"`
		PullRequest *struct {
			Number int  `json:"number"`
			Merged bool `json:"merged"`
		} `json:"pull_request"`
		Label *struct {
			Name string `json:"name"`
		} `json:"label"`
		Inputs map[string]any `json:"inputs"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return "", 0, err
	}
	switch name {
	case "pull_request", "pull_request_target":
		if ev.PullRequest == nil {
			return "", 0, nil
		}
		switch {
		case ev.Action == "closed" && ev.PullRequest.Merged:
			return "after-merge", ev.PullRequest.Number, nil
		case ev.Action == "dequeued":
			return "dequeued", ev.PullRequest.Number, nil
		case poolLabel != "" && ev.Action == "labeled" && ev.Label != nil && ev.Label.Name == poolLabel:
			return "schedule", 0, nil
		}
		return "", 0, nil
	case "pull_request_review", "workflow_run", "check_suite", "schedule":
		// An approval or a finished CI run can make a pool PR eligible, or
		// the last gate of a stack pass.
		if poolLabel != "" || autoLand {
			return "tick", 0, nil
		}
		return "", 0, nil
	case "workflow_dispatch":
		cmd := fmt.Sprint(ev.Inputs["command"])
		if cmd == "" || cmd == "<nil>" {
			cmd = "land"
		}
		if cmd == "schedule" || cmd == "tick" || cmd == "doctor" || cmd == "gc" {
			return cmd, 0, nil
		}
		n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(fmt.Sprint(ev.Inputs["pr"])), "#"))
		if err != nil {
			return "", 0, fmt.Errorf("workflow input pr must be a PR number")
		}
		return cmd, n, nil
	}
	return "", 0, nil
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var c common
	c.register(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	return runDoctor(r, &c)
}

func runDoctor(r *git.Repo, c *common) error {
	trunk := c.trunk(r, "")
	fmt.Printf("stackq doctor (trunk %s)\n\n", trunk)
	rs := doctor.Run(&forge.GH{Dir: r.Dir}, r.Dir, doctor.Config{
		Trunk: trunk, Prefix: c.cfg.Prefix, Landing: c.cfg.Landing, AutoLand: c.cfg.AutoLand,
		Priority: c.cfg.Priority != nil && c.cfg.Priority.Enabled,
	})
	if doctor.Print(os.Stdout, rs) {
		return errors.New("the setup has problems (✗ above)")
	}
	return nil
}

func cmdGC(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	var c common
	c.register(fs)
	del := fs.Bool("delete", false, "delete the branches (default: only list them)")
	trunkFlag := fs.String("trunk", "", "trunk branch (default: config, then the remote's HEAD)")
	if err := parse(fs, args); err != nil {
		return err
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	if c.remote == "" {
		return fmt.Errorf("no git remote found")
	}
	_, err = c.runner(r).GC(c.trunk(r, *trunkFlag), *del)
	return err
}
