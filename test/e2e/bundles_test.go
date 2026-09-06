package e2e

// The pattern bundles under bundles/ (Launch 10): every one simulates offline
// from a clean checkout — hashes verified, no simulation gap, nothing
// persisted — and its committed receipt.txt is what a fresh simulate prints.
// Patterns that are chains (qualify → group → send; the account shape) run
// their earlier bundles armed on the fixture AI engine between simulations,
// exactly as the README in each pattern directory says to; those armed runs
// are offline by construction (csv, sql, demo/enrich, fixture AI, group
// handoffs). Every manifest under bundles/ must belong to a chain here, so a
// new bundle cannot land untested.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// failedReason is the receipt's failure annotation (`fit: 3 failed — ai: …`),
// as distinct from a traverse line's `0 failed — 2 traversed`.
var failedReason = regexp.MustCompile(`(?m)^\S+: [1-9]\d* failed — `)

// patternChains lists every bundle, in the order its pattern runs them.
var patternChains = [][]string{
	{"email-waterfall"},
	{"posts-to-engagers"},
	{"events-cron"},
	{"qualify-group-send/1-qualify", "qualify-group-send/2-send"},
	{"account-shape/1-qualify", "account-shape/2-select", "account-shape/3-brief", "account-shape/4-outreach"},
}

func TestPatternBundlesSimulateFromACleanCheckout(t *testing.T) {
	root := filepath.Join(repoRoot(), "bundles")

	// Coverage: every frozen bundle in the tree is in a chain above.
	listed := map[string]bool{}
	for _, chain := range patternChains {
		for _, b := range chain {
			listed[b] = true
		}
	}
	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "manifest.json" {
			rel, _ := filepath.Rel(root, filepath.Dir(path))
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range found {
		if !listed[b] {
			t.Errorf("bundles/%s is frozen but not in patternChains — add it so CI simulates it", b)
		}
	}
	if len(found) != len(listed) {
		t.Errorf("found %d bundles on disk, %d listed", len(found), len(listed))
	}

	for _, chain := range patternChains {
		chain := chain
		t.Run(strings.Split(chain[0], "/")[0], func(t *testing.T) {
			h := newHarness(t)
			ai := h.fixtureScript("auto.json", "$auto")
			for i, name := range chain {
				dir := filepath.Join(h.work, filepath.FromSlash(name))
				copyTree(t, filepath.Join(root, filepath.FromSlash(name)), dir)

				// Simulate: zero keys, zero network, nothing persisted.
				sim := h.runIn(dir, nil, "", "run", ".", "--simulate")
				if sim.code != 0 {
					t.Fatalf("bundles/%s: simulate exit = %d\nstderr:\n%s", name, sim.code, sim.stderr)
				}
				contains(t, sim.stderr, "hashes verified", name+" bundle banner")
				contains(t, sim.stderr, "SIMULATED", name+" simulate receipt")
				if strings.Contains(sim.stderr, "simulation gap") {
					t.Errorf("bundles/%s: a step was not served from fixtures:\n%s", name, sim.stderr)
				}
				if failedReason.MatchString(sim.stderr) {
					t.Errorf("bundles/%s: a record failed in simulation:\n%s", name, sim.stderr)
				}
				// The receipt a clean checkout sees is the one committed.
				want := receiptTable(readFile(t, filepath.Join(dir, "receipt.txt")))
				got := receiptTable(sim.stderr)
				if want == "" || got == "" {
					t.Fatalf("bundles/%s: could not find the receipt table\nreceipt.txt:\n%s\nsimulate:\n%s", name,
						readFile(t, filepath.Join(dir, "receipt.txt")), sim.stderr)
				}
				if want != got {
					t.Errorf("bundles/%s: receipt.txt is stale — refreeze it (bundles/refreeze.py)\nwant:\n%s\ngot:\n%s", name, want, got)
				}

				// The chain's next bundle needs this one's group: run it armed,
				// on the fixture engine, spending and sending nothing.
				if i < len(chain)-1 {
					armed := h.runIn(dir, ai, "", "run", ".")
					if armed.code != 0 {
						t.Fatalf("bundles/%s: armed exit = %d\nstderr:\n%s", name, armed.code, armed.stderr)
					}
					// demo/enrich prices itself on purpose (ADR-056); nothing else
					// may have cost a cent — the fixture AI engine records $0 rows.
					if n := h.queryInt(`SELECT count(*) FROM costs WHERE provider != 'demo' AND amount_usd > 0`); n != 0 {
						t.Errorf("bundles/%s: the armed chain run recorded %d priced non-demo cost row(s)", name, n)
					}
				}
			}
			if n := h.queryInt(`SELECT count(*) FROM deliveries WHERE target NOT LIKE 'group:%'`); n != 0 {
				t.Errorf("the chain delivered %d record(s) somewhere other than a group", n)
			}
		})
	}
}

// receiptTable cuts the stable part of a receipt: from the step table's
// header through the total line. Run ids and timestamps live outside it.
func receiptTable(s string) string {
	lines := strings.Split(s, "\n")
	start, end := -1, -1
	for i, l := range lines {
		if start < 0 && strings.HasPrefix(l, "step ") {
			start = i
		}
		if start >= 0 && strings.HasPrefix(l, "total:") {
			end = i
			break
		}
	}
	if start < 0 || end < 0 {
		return ""
	}
	return strings.Join(lines[start:end+1], "\n")
}

// copyTree copies a bundle directory into the harness workspace.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
	if err != nil {
		t.Fatalf("copying %s: %v", src, err)
	}
}
