package land_test

import (
	"bytes"
	"strings"
	"testing"

	"stackq/internal/land"
	"stackq/internal/stack"
	"stackq/internal/testrepo"
)

func TestMarkersNamingAnotherTrunkAreIgnoredWhenTheTrunkIsKnown(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)

	var log bytes.Buffer
	good := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin", Trunk: "main", Log: &log}
	if err := good.AutoLand(); err != nil || len(fx.f.Merges) != 1 {
		t.Fatalf("markers that name the repository's trunk must work as before: %v %v\n%s", err, fx.f.Merges, log.String())
	}

	// A second stack whose bodies were rewritten, as a PR author could do.
	fx2 := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	prs, _ := fx2.f.ListPRs("all")
	for _, p := range prs {
		body := strings.ReplaceAll(p.Body, `"trunk":"main"`, `"trunk":"evil"`)
		if body == p.Body {
			t.Fatalf("test setup: no trunk in the marker of #%d: %s", p.Number, p.Body)
		}
		fx2.f.EditPR(p.Number, nil, &body)
	}
	log.Reset()
	bad := &land.Runner{Repo: fx2.dev, Forge: fx2.f, Remote: "origin", Trunk: "main", Log: &log}
	if err := bad.AutoLand(); err != nil || len(fx2.f.Merges) != 0 {
		t.Fatalf("a marker naming another trunk must not be acted on: %v %v", err, fx2.f.Merges)
	}
	if !strings.Contains(log.String(), `names trunk "evil"`) || !strings.Contains(log.String(), `trunk is "main"`) {
		t.Fatalf("the rejection should be logged:\n%s", log.String())
	}
	if n := strings.Count(log.String(), `names trunk "evil"`); n > len(prs) {
		t.Fatalf("each PR should be reported once, got %d reports for %d PRs", n, len(prs))
	}

	// Without a known trunk (local use) nothing changes.
	log.Reset()
	local := &land.Runner{Repo: fx2.dev, Forge: fx2.f, Remote: "origin", Log: &log}
	if err := local.AutoLand(); err != nil {
		t.Logf("local runner on a rewritten stack: %v", err)
	}
	if strings.Contains(log.String(), "ignoring it") {
		t.Fatalf("no trunk set, so nothing should be ignored:\n%s", log.String())
	}
}
