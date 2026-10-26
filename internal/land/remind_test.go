package land_test

import (
	"strings"
	"testing"
	"time"

	"stackq/internal/forge"
	"stackq/internal/land"
	"stackq/internal/stack"
	"stackq/internal/testrepo"
)

func TestActivationPRIsRemindedAfterAWeekAndThenEveryWeek(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	held := fx.s.Branches[3].PR
	other := fx.s.Branches[0].PR
	day := 24 * time.Hour
	ready := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	fx.f.AddLabels(held, land.LabelActivation)
	fx.f.AddLabels(other, land.LabelActivation) // a label on an ordinary part is ignored
	fx.f.ReadyFor = map[forge.ReadyKey]time.Time{
		{PR: held, Label: land.LabelActivation}:  ready,
		{PR: other, Label: land.LabelActivation}: ready,
	}
	now := ready.Add(6 * day)
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin", ReminderDays: 7, Now: func() time.Time { return now }}
	remind := func() int {
		t.Helper()
		before := len(fx.f.Comments[held])
		if err := run.RemindActivations(); err != nil {
			t.Fatal(err)
		}
		return len(fx.f.Comments[held]) - before
	}

	if remind() != 0 {
		t.Fatal("reminded after only 6 days")
	}
	now = ready.Add(8 * day)
	if remind() != 1 {
		t.Fatal("no reminder after 8 days")
	}
	said := fx.f.Comments[held][len(fx.f.Comments[held])-1]
	for _, want := range []string{"landed 8 days ago", "Stack `feat`", "stackq land --pr", "again in 7 days"} {
		if !strings.Contains(said, want) {
			t.Errorf("reminder lacks %q: %s", want, said)
		}
	}
	if !fx.f.Labels[held][land.LabelReminded] {
		t.Fatal("the reminded label is how the next reminder is timed, but it is missing")
	}
	if len(fx.f.Comments[other]) != 0 {
		t.Fatalf("an ordinary part was reminded about: %v", fx.f.Comments[other])
	}

	// The label's time is the new start of the wait.
	fx.f.ReadyFor[forge.ReadyKey{PR: held, Label: land.LabelReminded}] = now
	now = now.Add(3 * day)
	if remind() != 0 {
		t.Fatal("reminded again 3 days after the last reminder")
	}
	now = now.Add(5 * day)
	if remind() != 1 {
		t.Fatal("no second reminder a week after the first")
	}
}

func TestActivationRemindersCanBeSwitchedOff(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	held := fx.s.Branches[3].PR
	fx.f.AddLabels(held, land.LabelActivation)
	fx.f.ReadyFor = map[forge.ReadyKey]time.Time{{PR: held, Label: land.LabelActivation}: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin", ReminderDays: 0}
	if err := run.RemindActivations(); err != nil || len(fx.f.Comments[held]) != 0 {
		t.Fatalf("reminders are off but got %v %v", err, fx.f.Comments[held])
	}
}
