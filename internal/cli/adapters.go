package cli

// gtme adapters — the ADR-042 verb set (SPEC §8): list installed adapters
// with source and pin, search the registry index, install a binding from a
// pinned URL (verified first — nothing installs unverified), verify one any
// time, and move a pin explicitly. All human-facing output goes to stderr.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/elegant-atomics/gtme/internal/adapterinstall"
	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/binding"
	"github.com/elegant-atomics/gtme/internal/identity"
	"github.com/elegant-atomics/gtme/internal/protocol"
)

func cmdAdapters(ctx context.Context, env Env, args []string) error {
	if len(args) == 0 {
		return adaptersList(env)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "search":
		if len(rest) != 1 {
			return fail(ExitValidation, "usage: gtme adapters search TEXT")
		}
		return adaptersSearch(env, rest[0])
	case "add":
		if len(rest) != 1 {
			return fail(ExitValidation, "usage: gtme adapters add github.com/<owner>/<repo>/<path>[@ref]")
		}
		return adaptersAdd(env, rest[0])
	case "verify":
		if len(rest) != 1 {
			return fail(ExitValidation, "usage: gtme adapters verify ID")
		}
		dir, err := installedBindingDir(rest[0])
		if err != nil {
			return err
		}
		_, err = verifyBindingDir(env, dir)
		return err
	case "update":
		if len(rest) < 1 || len(rest) > 2 {
			return fail(ExitValidation, "usage: gtme adapters update ID [@ref]")
		}
		newRef := ""
		if len(rest) == 2 {
			newRef = strings.TrimPrefix(rest[1], "@")
		}
		return adaptersUpdate(env, rest[0], newRef)
	default:
		return fail(ExitValidation,
			"usage: gtme adapters [search TEXT | add REF | verify ID | update ID [@ref]]")
	}
}

// installDir is where `add` puts a binding: the home half of the §6 search
// path (never GTME_ADAPTER_PATH, which is the operator's own overlay).
func installDir() (string, error) {
	if home := os.Getenv("GTME_HOME"); home != "" {
		return filepath.Join(home, "adapters"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fail(ExitOther, "resolving home: %v", err)
	}
	return filepath.Join(home, ".gtme", "adapters"), nil
}

// installedBindingDir finds an installed binding by id on the §6 search path.
func installedBindingDir(id string) (string, error) {
	for _, root := range adapters.SearchPath() {
		for _, name := range []string{filepath.FromSlash(id), strings.ReplaceAll(id, "/", "-")} {
			dir := filepath.Join(root, name)
			if _, err := os.Stat(filepath.Join(dir, "binding.yaml")); err == nil {
				return dir, nil
			}
		}
	}
	return "", fail(ExitValidation, "adapters: %q is not an installed binding (searched %s)",
		id, strings.Join(adapters.SearchPath(), ", "))
}

// adaptersList prints every externally installed adapter with its source and
// pin (SPEC §8). Built-ins are the floor and are listed by `help --agent`;
// this verb answers "what did I install, and from where".
func adaptersList(env Env) error {
	type row struct{ id, version, role, kind, source string }
	var rows []row
	seen := map[string]bool{}
	for _, root := range adapters.SearchPath() {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			dir := filepath.Join(root, e.Name())
			r := row{kind: "binding"}
			if raw, err := os.ReadFile(filepath.Join(dir, "binding.yaml")); err == nil {
				b, err := binding.Parse(raw)
				if err != nil {
					r.id, r.source = e.Name(), "unparseable: "+truncate(err.Error(), 60)
					rows = append(rows, r)
					seen[e.Name()] = true
					continue
				}
				r.id, r.version, r.role = b.ID, fmt.Sprint(b.Version), b.Role
			} else if raw, err := os.ReadFile(filepath.Join(dir, "manifest.json")); err == nil {
				m, err := adapters.ParseManifest(raw)
				if err != nil {
					continue
				}
				r.id, r.version, r.role, r.kind = m.ID, fmt.Sprint(m.Version), m.Role, "process"
			} else {
				continue
			}
			src, err := adapterinstall.ReadSource(dir)
			if err != nil {
				return err
			}
			if src == nil {
				r.source = "installed by hand"
			} else {
				r.source = fmt.Sprintf("%s/%s@%s (%s)", src.URL, src.Path, refOrHead(src.Ref), shortCommit(src.Commit))
			}
			rows = append(rows, r)
			seen[e.Name()] = true
		}
	}
	if len(rows) == 0 {
		fmt.Fprintln(env.Stderr, "no external adapters installed — see `gtme adapters search` and `gtme help --bindings`")
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	tw := tabwriter.NewWriter(env.Stderr, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tVERSION\tROLE\tKIND\tSOURCE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.id, r.version, r.role, r.kind, r.source)
	}
	return tw.Flush()
}

func adaptersSearch(env Env, q string) error {
	ix, err := adapterinstall.LoadIndex()
	if err != nil {
		return fail(ExitNetwork, "%v", err)
	}
	hits := ix.Search(q)
	if len(hits) == 0 {
		fmt.Fprintf(env.Stderr, "no registry entries match %q (index: %s)\nan agent that finds nothing writes a binding — `gtme help --bindings`\n",
			q, adapterinstall.RegistryURL())
		return nil
	}
	tw := tabwriter.NewWriter(env.Stderr, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tROLE\tTIER\tINSTALL\tDESCRIPTION")
	for _, e := range hits {
		install := fmt.Sprintf("gtme adapters add %s/%s@%s", e.Source.URL, e.Source.Path, refOrHead(e.Source.Ref))
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.ID, e.Role, e.Tier, install, truncate(e.Description, 60))
	}
	return tw.Flush()
}

func adaptersAdd(env Env, refStr string) error {
	ref, err := adapterinstall.ParseRef(refStr)
	if err != nil {
		return fail(ExitValidation, "%v", err)
	}
	dir, b, hash, commit, err := fetchAndVerify(env, ref)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	root, err := installDir()
	if err != nil {
		return err
	}
	dest := filepath.Join(root, strings.ReplaceAll(b.ID, "/", "-"))
	if _, err := os.Stat(dest); err == nil {
		return fail(ExitValidation, "adapters: %s is already installed at %s — `gtme adapters update %s` moves the pin", b.ID, dest, b.ID)
	}
	if err := installTree(dir, dest); err != nil {
		return err
	}
	if err := adapterinstall.WriteSource(dest, adapterinstall.Source{
		URL: ref.URL(), Path: ref.Path, Ref: ref.Ref, Commit: commit, SHA256: hash,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "installed %s at %s — pinned to %s\n", b.ID, dest, shortCommit(commit))
	return nil
}

func adaptersUpdate(env Env, id, newRef string) error {
	dir, err := installedBindingDir(id)
	if err != nil {
		return err
	}
	src, err := adapterinstall.ReadSource(dir)
	if err != nil {
		return err
	}
	if src == nil {
		return fail(ExitValidation, "adapters: %s was installed by hand (no %s) — nothing to update from", id, adapterinstall.SourceFile)
	}
	owner, repo, ok := splitRepoURL(src.URL)
	if !ok {
		return fail(ExitValidation, "adapters: %s: unrecognized source url %q", id, src.URL)
	}
	ref := adapterinstall.Ref{Owner: owner, Repo: repo, Path: src.Path, Ref: src.Ref}
	if newRef != "" {
		ref.Ref = newRef
	}
	tmp, b, hash, commit, err := fetchAndVerify(env, ref)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if b.ID != id {
		return fail(ExitValidation, "adapters: %s now declares id %q — remove and re-add instead", id, b.ID)
	}
	if commit == src.Commit {
		fmt.Fprintf(env.Stderr, "%s is already at %s — pin unchanged\n", id, shortCommit(commit))
		return nil
	}
	staging := dir + ".update"
	if err := installTree(tmp, staging); err != nil {
		return err
	}
	if err := adapterinstall.WriteSource(staging, adapterinstall.Source{
		URL: src.URL, Path: ref.Path, Ref: ref.Ref, Commit: commit, SHA256: hash,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		os.RemoveAll(staging)
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		os.RemoveAll(staging)
		return err
	}
	if err := os.Rename(staging, dir); err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "updated %s — pin moved %s → %s\n", id, shortCommit(src.Commit), shortCommit(commit))
	return nil
}

// fetchAndVerify is the shared front half of add and update: resolve, fetch,
// verify (nothing installs unverified), hash, and check the hash against the
// registry index when the index publishes this source (SPEC §11 M19).
func fetchAndVerify(env Env, ref adapterinstall.Ref) (dir string, b *binding.Binding, hash, commit string, err error) {
	commit, err = adapterinstall.ResolveCommit(ref)
	if err != nil {
		return "", nil, "", "", fail(ExitNetwork, "%v", err)
	}
	dir, err = adapterinstall.FetchDir(ref, commit)
	if err != nil {
		return "", nil, "", "", fail(ExitNetwork, "%v", err)
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()
	fmt.Fprintf(env.Stderr, "fetched %s at %s\n", ref.String(), shortCommit(commit))
	b, err = verifyBindingDir(env, dir)
	if err != nil {
		return "", nil, "", "", err
	}
	hash, err = adapterinstall.ContentHash(dir)
	if err != nil {
		return "", nil, "", "", err
	}
	if ix, ierr := adapterinstall.LoadIndex(); ierr != nil {
		fmt.Fprintf(env.Stderr, "warning: registry index unreachable, content-hash check skipped: %v\n", ierr)
	} else if e := ix.FindSource(ref.URL(), ref.Path); e != nil && e.SHA256 != hash {
		return "", nil, "", "", fail(ExitValidation,
			"adapters: content hash mismatch for %s — the index lists %s, the fetched directory hashes to %s; the thing reviewed is not the thing fetched, refusing to install",
			ref.String(), shortCommit(e.SHA256), shortCommit(hash))
	}
	ok = true
	return dir, b, hash, commit, nil
}

// verifyBindingDir is `gtme adapters verify` (SPEC §8): schema, the
// reviewable surface (hosts, credentials, needs/provides), and the
// conformance fixtures run offline through the real engine. Fixtures are
// mandatory; a binding that ships none, or whose fixtures fail, fails here —
// and `add` runs this before anything installs.
func verifyBindingDir(env Env, dir string) (*binding.Binding, error) {
	b, fixtures, err := binding.LoadFS(os.DirFS(dir))
	if err != nil {
		return nil, fail(ExitValidation, "adapters: %v", err)
	}
	m, err := b.Manifest()
	if err != nil {
		return nil, fail(ExitValidation, "adapters: %s: %v", b.ID, err)
	}
	// An entity_type with no §4 identity derivation can never enter the
	// ledger — refusing here closes the gap between "certified" and "works",
	// instead of the runner dropping every record after a paid call (#27).
	if m.EntityType != "" && !identity.Supported(m.EntityType) {
		return nil, fail(ExitValidation,
			"adapters: %s: entity_type %q has no identity derivation in this build (SPEC §4 defines %s) — its records would all be dropped at the identity boundary",
			b.ID, m.EntityType, strings.Join(identity.SupportedTypes(), ", "))
	}

	fmt.Fprintf(env.Stderr, "%s v%d — %s (%s)\n", b.ID, b.Version, b.Role, m.EntityType)
	fmt.Fprintf(env.Stderr, "  calls:       %s\n", bindingHost(b))
	creds := "none"
	if len(m.Credentials) > 0 {
		creds = strings.Join(m.Credentials, ", ")
	}
	if len(m.CredentialsOptional) > 0 {
		creds += " (optional: " + strings.Join(m.CredentialsOptional, ", ") + ")"
	}
	fmt.Fprintf(env.Stderr, "  demands:     %s\n", creds)
	fmt.Fprintf(env.Stderr, "  needs:       %s\n", schemaSummary(m.Needs))
	fmt.Fprintf(env.Stderr, "  provides:    %s\n", schemaSummary(m.Provides))

	if fixtures == nil {
		return nil, fail(ExitValidation,
			"adapters: %s ships no conformance fixtures (%s) — fixtures are mandatory; record one real response per request (`gtme help --bindings`)", b.ID, binding.FixtureFile)
	}
	records, err := runFixtures(b, m, fixtures)
	if err != nil {
		return nil, fail(ExitValidation, "adapters: %s: fixtures failed: %v", b.ID, err)
	}
	fmt.Fprintf(env.Stderr, "  fixtures:    ok — %d response(s) on file, %d record(s) extracted\n", len(fixtures.Responses), records)
	return b, nil
}

// runFixtures drives the binding through the real engine with the fixture
// set as its HTTP seam — the conformance-kit shape, minus testing.T.
func runFixtures(b *binding.Binding, m *adapters.Manifest, fixtures *binding.FixtureSet) (int, error) {
	cfg := fixtures.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	if missing := missingRequiredConfig(b.ConfigSchema, cfg); len(missing) > 0 {
		return 0, fmt.Errorf("config requires %s — add a `config` member to %s so verify can drive the run",
			strings.Join(missing, ", "), binding.FixtureFile)
	}
	var input []protocol.Message
	if b.Role != adapters.RoleSource {
		if fixtures.Input == nil {
			return 0, fmt.Errorf("a %s binding consumes records — add an `input` member (sample fields) to %s so verify can drive the run",
				b.Role, binding.FixtureFile)
		}
		key := "verify"
		if v, ok := fixtures.Input["email"].(string); ok {
			key = v
		}
		input = append(input, protocol.Record(protocol.Key{EntityType: m.EntityType, IdentityKey: key}, fixtures.Input, nil))
	}
	credEnv := map[string]string{}
	for _, c := range append(append([]string{}, m.Credentials...), m.CredentialsOptional...) {
		credEnv[c] = "fixture-verify"
	}

	eng := &binding.Engine{B: b, HTTP: fixtures.Doer()}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		w := protocol.NewWriter(inW)
		w.Write(protocol.Message{Type: protocol.TypeOpen, StepID: "verify", RunID: "verify", Config: cfg})
		for _, rec := range input {
			w.Write(rec)
		}
		w.Write(protocol.End())
		inW.Close()
	}()
	runErr := make(chan error, 1)
	var logs strings.Builder
	go func() {
		err := eng.Run(context.Background(), adapters.Ports{In: inR, Out: outW, Log: &logs, Env: credEnv})
		outW.CloseWithError(err)
		runErr <- err
	}()
	records := 0
	r := protocol.NewReader(outR)
	for {
		m, err := r.Next()
		if err != nil {
			break
		}
		if m.Type == protocol.TypeRecord {
			records++
		}
	}
	if err := <-runErr; err != nil {
		if logs.Len() > 0 {
			return records, fmt.Errorf("%v\n%s", err, strings.TrimSpace(logs.String()))
		}
		return records, err
	}
	if b.Role == adapters.RoleSource && records == 0 {
		return 0, fmt.Errorf("the fixtures produced no records — a source's fixtures must yield at least one")
	}
	return records, nil
}

// bindingHost renders the request URL with config defaults applied and
// reports its host — the reviewable "what will this call" line. A host that
// only resolves at run time is shown as its template.
func bindingHost(b *binding.Binding) string {
	u := b.Request.URL
	var schema struct {
		Properties map[string]struct {
			Default any `json:"default"`
		} `json:"properties"`
	}
	if len(b.ConfigSchema) > 0 {
		_ = json.Unmarshal(b.ConfigSchema, &schema)
	}
	for name, p := range schema.Properties {
		if p.Default != nil {
			u = strings.ReplaceAll(u, "{{config."+name+"}}", fmt.Sprint(p.Default))
		}
	}
	if !strings.Contains(u, "{{") {
		if parsed, err := url.Parse(u); err == nil && parsed.Host != "" {
			return parsed.Host
		}
	}
	return u
}

func missingRequiredConfig(schemaRaw json.RawMessage, cfg map[string]any) []string {
	if len(schemaRaw) == 0 {
		return nil
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schemaRaw, &schema); err != nil {
		return nil
	}
	var missing []string
	for _, k := range schema.Required {
		if _, ok := cfg[k]; !ok {
			missing = append(missing, k)
		}
	}
	return missing
}

func schemaSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "none"
	}
	var s struct {
		Required   []string       `json:"required"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "unreadable"
	}
	var names []string
	for k := range s.Properties {
		names = append(names, k)
	}
	sort.Strings(names)
	req := map[string]bool{}
	for _, k := range s.Required {
		req[k] = true
	}
	for i, n := range names {
		if req[n] {
			names[i] = n + "*"
		}
	}
	if len(names) == 0 {
		return "none declared"
	}
	return strings.Join(names, ", ") + " (* required)"
}

func installTree(src, dest string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
}

func splitRepoURL(u string) (owner, repo string, ok bool) {
	parts := strings.Split(u, "/")
	if len(parts) != 3 || parts[0] != "github.com" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func refOrHead(ref string) string {
	if ref == "" {
		return "HEAD"
	}
	return ref
}

func shortCommit(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
