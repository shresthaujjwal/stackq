package land

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"stackq/internal/plural"
)

// DefaultReminderDays is how long a ready activation PR waits before stackq
// reminds the team, and then between reminders.
const DefaultReminderDays = 7

// RemindActivations comments on activation PRs that have waited for a human
// longer than ReminderDays, then again every ReminderDays. An activation PR
// is held on purpose, so nothing else would ever mention it again; left
// alone they pile up (seven were open in the sandbox shop at once).
//
// It needs no state of its own: the time the activation label was added says
// when the wait began, and the stackq:reminded label, removed and added again
// at each reminder, says when it last spoke. Safe to run on every tick.
func (c *Runner) RemindActivations() error {
	if c.ReminderDays <= 0 {
		return nil
	}
	wait := time.Duration(c.ReminderDays) * 24 * time.Hour
	held, err := c.Forge.ListPool(LabelActivation)
	if err != nil {
		return err
	}
	sort.Slice(held, func(i, j int) bool { return held[i].Number < held[j].Number })
	var errs []error
	for _, p := range held {
		// A label can be added by anyone with triage rights; only PRs that
		// really are the held part of a stack get a comment.
		m, ok := c.markerOf(p)
		if !ok || !m.Hold {
			continue
		}
		ready, err := c.Forge.ReadyAt(p.Number, LabelActivation)
		if err != nil {
			errs = append(errs, fmt.Errorf("#%d: %w", p.Number, err))
			continue
		}
		if ready.IsZero() {
			continue
		}
		since := ready
		if hasLabel(p.Labels, LabelReminded) {
			if last, err := c.Forge.ReadyAt(p.Number, LabelReminded); err == nil && last.After(since) {
				since = last
			}
		}
		if c.now().Sub(since) < wait {
			continue
		}
		days := int(c.now().Sub(ready) / (24 * time.Hour))
		if err := c.Forge.Comment(p.Number, fmt.Sprintf("⏰ Stack `%s` landed %s ago and this activation PR is still waiting. "+
			"Merge it (or run `stackq land --pr %d`) when you are ready to switch the feature on, or close it if it is no longer needed. "+
			"stackq reminds again in %s.", m.Stack, plural.N(days, "day"), p.Number, plural.N(c.ReminderDays, "day"))); err != nil {
			errs = append(errs, fmt.Errorf("#%d: %w", p.Number, err))
			continue
		}
		// Remove first so adding it again records a new time.
		c.Forge.RemoveLabels(p.Number, LabelReminded)
		if err := c.Forge.AddLabels(p.Number, LabelReminded); err != nil {
			errs = append(errs, fmt.Errorf("#%d: %w", p.Number, err))
			continue
		}
		c.logf("⏰ #%d (activation) has waited %s; reminded", p.Number, plural.N(days, "day"))
	}
	return errors.Join(errs...)
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
