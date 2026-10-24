package land_test

import (
	"strings"
	"testing"
	"time"

	"stackq/internal/forge"
	"stackq/internal/land"
	"stackq/internal/prio"
	"stackq/internal/stack"
	"stackq/internal/testrepo"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func prioCfg() *prio.Config {
	c := prio.Config{Enabled: true}.WithDefaults()
	c.MaxInFlight = 3
	return &c
}

func addPR(f *forge.Fake, head, base, title, body, author string, lines, files int, age time.Duration, labels ...string) int {
	p, _ := f.CreatePR(head, base, title, body, false)
	pr := f.PRs[p.Number]
	pr.Author, pr.Additions, pr.ChangedFiles, pr.CreatedAt = author, lines, files, t0.Add(-age)
	f.AddLabels(p.Number, append([]string{"stackq:queue"}, labels...)...)
	return p.Number
}

func queueOrder(f *forge.Fake) []int {
	q, _ := f.Queue("main")
	var out []int
	for _, e := range q {
		out = append(out, e.PR)
	}
	return out
}

func TestSchedulerPutsVulnAndSmallFirst(t *testing.T) {
	f := forge.NewFake()
	human := addPR(f, "someone/manual", "main", "queued by hand", "", "ana", 200, 9, 3*time.Hour)
	f.RemoveLabels(human, "stackq:queue")
	f.Enqueue(forge.PR{Number: human}, forge.EnqueueOptions{})

	big := addPR(f, "feat/big", "main", "New billing engine", "", "bo", 900, 30, 2*time.Hour)
	typo := addPR(f, "fix/typo", "main", "Fix typo in README", "", "cy", 2, 1, 5*time.Minute)
	bot := addPR(f, "dependabot/npm_and_yarn/lodash-4.17.21", "main", "Bump lodash from 4.17.20 to 4.17.21", "", "dependabot[bot]", 40, 2, 10*time.Minute)
	vuln := addPR(f, "fix/openssl", "main", "Upgrade openssl (CVE-2026-31337)", "", "di", 300, 12, time.Minute)
	pending := addPR(f, "fix/typo2", "main", "Another typo", "", "cy", 1, 1, time.Hour)
	f.SetDetails(pending, func(d *forge.Details) { d.Checks = "pending" })
	draft := addPR(f, "wip/x", "main", "WIP", "", "cy", 1, 1, time.Hour)
	f.PRs[draft].Draft = true
	other := addPR(f, "x/y", "release-1.2", "Backport", "", "cy", 1, 1, time.Hour)

	run := &land.Runner{Forge: f, Prio: prioCfg(), Now: func() time.Time { return t0 }}
	ds, err := run.Schedule("main", false)
	if err != nil {
		t.Fatal(err)
	}
	// 1 in flight: the CVE fix jumps (2), the dependabot bump fills the last slot (3).
	if got := queueOrder(f); len(got) != 3 || got[0] != vuln || got[1] != human || got[2] != bot {
		t.Fatalf("queue = %v, want [%d %d %d]", got, vuln, human, bot)
	}
	if len(f.Jumped) != 1 || !f.Labels[vuln][forge.LabelJumped] || !f.Labels[vuln]["stackq:prio:urgent"] || f.Labels[vuln]["stackq:queue"] {
		t.Fatalf("vuln labels: %v", f.Labels[vuln])
	}
	if c := f.Comments[vuln]; len(c) != 1 || !strings.Contains(c[0], "jumped to the front") || !strings.Contains(c[0], "CVE-2026-31337") {
		t.Fatalf("vuln comment: %q", c)
	}
	why := map[int]string{}
	act := map[int]prio.Action{}
	for _, d := range ds {
		act[d.PR.Number], why[d.PR.Number] = d.Action, d.Note
	}
	for n, a := range map[int]prio.Action{typo: prio.Wait, big: prio.Wait, pending: prio.Skip, draft: prio.Skip, other: prio.Skip} {
		if act[n] != a {
			t.Errorf("#%d: %s (%s), want %s", n, act[n], why[n], a)
		}
	}
	if !strings.Contains(why[pending], "checks running") || !strings.Contains(why[other], "release-1.2") {
		t.Fatalf("skip reasons: %v", why)
	}

	// Two entries land. Both small PRs go before the big feature; the one
	// that has waited an hour goes before the fresh typo fix.
	f.MarkMerged(human)
	f.Dequeue(human)
	f.MarkMerged(vuln)
	f.Dequeue(vuln)
	f.SetDetails(pending, func(d *forge.Details) { d.Checks = "success" })
	run.Now = func() time.Time { return t0.Add(time.Minute) }
	if _, err := run.Schedule("main", false); err != nil {
		t.Fatal(err)
	}
	if got := queueOrder(f); len(got) != 3 || got[0] != bot || got[1] != pending || got[2] != typo {
		t.Fatalf("second pass queue = %v, want [%d %d %d]", got, bot, pending, typo)
	}
	if !f.Labels[big]["stackq:queue"] {
		t.Fatal("the big PR should still be waiting in the pool")
	}
}

func TestSchedulerAgingLetsBigPRThrough(t *testing.T) {
	f := forge.NewFake()
	big := addPR(f, "feat/big", "main", "Big", "", "bo", 900, 30, 12*time.Hour) // 24 aging points
	small := addPR(f, "fix/a", "main", "Small", "", "cy", 3, 1, time.Minute)
	cfg := prioCfg()
	cfg.MaxInFlight = 1
	run := &land.Runner{Forge: f, Prio: cfg, Now: func() time.Time { return t0 }}
	if _, err := run.Schedule("main", false); err != nil {
		t.Fatal(err)
	}
	if got := queueOrder(f); len(got) != 1 || got[0] != big {
		t.Fatalf("aged big PR should win the only slot, queue = %v (small #%d)", got, small)
	}
}

func TestSchedulerDryRunChangesNothing(t *testing.T) {
	f := forge.NewFake()
	addPR(f, "fix/a", "main", "fix CVE-2026-0001", "", "cy", 3, 1, time.Minute)
	run := &land.Runner{Forge: f, Prio: prioCfg(), Now: func() time.Time { return t0 }}
	ds, err := run.Schedule("main", true)
	if err != nil || len(ds) != 1 || ds[0].Action != prio.Jump || len(f.Enqueued) != 0 {
		t.Fatalf("dry run: %v %+v %v", err, ds, f.Enqueued)
	}
}

func TestAtomicLandAndRevertGoThroughPriority(t *testing.T) {
	fx := setupWith(t, testrepo.GoApp(t), nil, stack.Atomic)
	cfg := prioCfg()
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin", Prio: cfg, Now: func() time.Time { return t0 }}

	// A part of the atomic stack labelled by mistake is never queued alone.
	part := prBy(t, fx, 1)
	fx.f.AddLabels(part.Number, cfg.Label)

	if err := run.Land(prBy(t, fx, 0).Number); err != nil {
		t.Fatal(err)
	}
	top := prBy(t, fx, len(fx.s.Branches)-1)
	if got := queueOrder(fx.f); len(got) != 1 || got[0] != top.Number {
		t.Fatalf("only the lander may be queued, queue = %v", got)
	}
	if !fx.f.Labels[part.Number][cfg.Label] {
		t.Fatal("the stray part should stay in the pool, skipped")
	}

	if err := fx.queueLands(t, top.Number, "rebase"); err != nil {
		t.Fatal(err)
	}
	fx.f.Dequeue(top.Number)
	fx.f.RemoveLabels(part.Number, cfg.Label)

	// Something big is already queued; the revert still jumps ahead of it.
	big := addPR(fx.f, "feat/big", "main", "Big", "", "bo", 900, 30, time.Hour)
	fx.f.RemoveLabels(big, cfg.Label)
	fx.f.Enqueue(forge.PR{Number: big}, forge.EnqueueOptions{})

	rrun := &land.Runner{Repo: testrepo.Clone(t, fx.bare), Forge: fx.f, Remote: "origin", Prio: cfg, Now: func() time.Time { return t0 }}
	rev, err := rrun.Revert(top.Number, land.RevertOptions{Enqueue: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := queueOrder(fx.f); len(got) != 2 || got[0] != rev.Number || !fx.f.Labels[rev.Number]["stackq:prio:urgent"] {
		t.Fatalf("revert should jump the queue: %v labels=%v", got, fx.f.Labels[rev.Number])
	}
}
