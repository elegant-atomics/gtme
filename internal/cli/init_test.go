package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Args: args})
	return code, out.String(), errb.String()
}

func TestInitCreatesLedgerAndIsRepeatable(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, "ledger.db")
	t.Setenv("GTM_LEDGER", dbPath)

	code, stdout, stderr := runCLI(t, "init")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay data-only, got %q", stdout)
	}
	if !strings.Contains(stderr, dbPath) {
		t.Errorf("stderr should name the ledger path, got %q", stderr)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("ledger not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "adapters")); err != nil {
		t.Errorf("adapters dir not created: %v", err)
	}

	if code, _, stderr := runCLI(t, "init"); code != ExitOK {
		t.Fatalf("second init exit = %d, stderr: %s", code, stderr)
	} else if !strings.Contains(stderr, "up to date") {
		t.Errorf("second init should report an existing ledger, got %q", stderr)
	}
}

func TestInitRejectsArguments(t *testing.T) {
	t.Setenv("GTM_LEDGER", filepath.Join(t.TempDir(), "ledger.db"))
	if code, _, _ := runCLI(t, "init", "extra"); code != ExitValidation {
		t.Errorf("exit = %d, want %d", code, ExitValidation)
	}
}

func TestUnknownAndMissingCommand(t *testing.T) {
	if code, _, stderr := runCLI(t, "frobnicate"); code != ExitValidation {
		t.Errorf("exit = %d, want %d (stderr %q)", code, ExitValidation, stderr)
	}
	if code, _, _ := runCLI(t); code != ExitValidation {
		t.Errorf("bare gtm exit = %d, want %d", code, ExitValidation)
	}
}

func TestEveryVerbIsImplemented(t *testing.T) {
	t.Setenv("GTM_LEDGER", filepath.Join(t.TempDir(), "ledger.db"))
	runCLI(t, "init")

	// No verb should report itself unimplemented any more.
	for _, args := range [][]string{
		{"runs"}, {"secret", "list"}, {"query", "--list"}, {"freeze"},
		{"plan"}, {"run"}, {"source"}, {"filter"}, {"enrich"}, {"compose"}, {"deliver"},
	} {
		_, _, stderr := runCLI(t, args...)
		if strings.Contains(stderr, "not implemented") {
			t.Errorf("gtm %s reports itself unimplemented: %q", strings.Join(args, " "), stderr)
		}
	}
}

func TestPlanAndRunNeedAPipelineArgument(t *testing.T) {
	for _, verb := range []string{"plan", "run"} {
		code, _, stderr := runCLI(t, verb)
		if code != ExitValidation {
			t.Errorf("%s exit = %d, want %d", verb, code, ExitValidation)
		}
		if !strings.Contains(stderr, "usage: gtm "+verb) {
			t.Errorf("%s stderr = %q", verb, stderr)
		}
	}
}

func TestVersion(t *testing.T) {
	code, stdout, stderr := runCLI(t, "version")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "gtm ") {
		t.Errorf("stderr = %q", stderr)
	}
}
