// Package secrets resolves adapter credentials: the OS environment first, then
// ~/.gtm/secrets (a KEY=value file, mode 0600) (SPEC §6).
package secrets

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/trevorfox/gtm/internal/ledger"
)

// Path returns the secrets file path.
func Path() (string, error) {
	home, err := ledger.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "secrets"), nil
}

// Lookup resolves one credential.
func Lookup(name string) (string, bool) {
	if v := os.Getenv(name); v != "" {
		return v, true
	}
	file, err := load()
	if err != nil {
		return "", false
	}
	v, ok := file[name]
	return v, ok && v != ""
}

// Resolve looks up every named credential, reporting which are missing.
func Resolve(names []string) (map[string]string, []string) {
	found := map[string]string{}
	var missing []string
	for _, n := range names {
		if v, ok := Lookup(n); ok {
			found[n] = v
			continue
		}
		missing = append(missing, n)
	}
	sort.Strings(missing)
	return found, missing
}

// Set writes a credential to the secrets file, creating it 0600.
func Set(name, value string) error {
	if strings.ContainsAny(name, "=\n") || name == "" {
		return fmt.Errorf("secrets: invalid key %q", name)
	}
	if strings.ContainsAny(value, "\n") {
		return fmt.Errorf("secrets: value for %s must not contain a newline", name)
	}
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("secrets: creating %s: %w", filepath.Dir(path), err)
	}
	current, err := load()
	if err != nil {
		return err
	}
	current[name] = value

	keys := make([]string, 0, len(current))
	for k := range current {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf strings.Builder
	buf.WriteString("# gtm secrets — written by `gtm secret set`. Mode 0600.\n")
	for _, k := range keys {
		buf.WriteString(k + "=" + current[k] + "\n")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o600); err != nil {
		return fmt.Errorf("secrets: writing %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("secrets: writing %s: %w", path, err)
	}
	return nil
}

// Names lists the keys held in the secrets file (never the values).
func Names() ([]string, error) {
	file, err := load()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(file))
	for k := range file {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func load() (map[string]string, error) {
	out := map[string]string{}
	path, err := Path()
	if err != nil {
		return out, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, fmt.Errorf("secrets: reading %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("secrets: reading %s: %w", path, err)
	}
	return out, nil
}
