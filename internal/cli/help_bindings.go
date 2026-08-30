package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"

	"github.com/elegant-atomics/gtme/internal/adapters"
	"github.com/elegant-atomics/gtme/internal/binding"
	"github.com/elegant-atomics/gtme/spec"
)

// cmdHelpBindings emits the second agent surface (SPEC §8, ADR-041): the
// binding contract, for an agent that needs an adapter gtme does not ship.
// `help --agent` is the pipeline document and points here; this one carries
// the schema, the discovery path, one shipped binding verbatim as the worked
// example, the fixtures expectation, and the verbs that touch bindings.
// Regenerated from the embedded artifacts, never hand-maintained: the schema
// is spliced in byte for byte, the reference is whichever shipped binding is
// smallest, the search path is the live one.
//
// Acceptance (SPEC §8): an agent given only this document must be able to
// author a binding that `gtme plan` resolves — so everything an author needs
// to place, name and prove a binding is here, not just the schema.
func cmdHelpBindings(env Env) error {
	doc, err := bindingsSurface()
	if err != nil {
		return fail(ExitOther, "assembling bindings doc: %v", err)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // `<id>` reads as written, not as <id>
	if err := enc.Encode(doc); err != nil {
		return fail(ExitOther, "writing bindings doc: %v", err)
	}
	// json.Encoder compacts a RawMessage; SPEC §11 M18 wants the artifact
	// byte for byte. Splice it in as the last member instead.
	out.Truncate(len(bytes.TrimSuffix(bytes.TrimSpace(out.Bytes()), []byte("}"))))
	out.Truncate(len(bytes.TrimRight(out.Bytes(), "\n")))
	out.WriteString(",\n  \"schema\": ")
	out.Write(bytes.TrimSpace(spec.BindingSchema))
	out.WriteString("\n}\n")
	if _, err := env.Stdout.Write(out.Bytes()); err != nil {
		return fail(ExitOther, "writing bindings doc: %v", err)
	}
	return nil
}

type bindingsDoc struct {
	Note      string            `json:"note"`
	Discovery bindingsDiscovery `json:"discovery"`
	Fixtures  bindingsFixtures  `json:"fixtures"`
	Reference bindingsReference `json:"reference"`
	Verbs     []agentVerb       `json:"verbs"`
	Registry  bindingsRegistry  `json:"registry"`
	// Schema (spec/binding-schema.json) is spliced in verbatim by
	// cmdHelpBindings, after these, so the artifact survives byte for byte.
}

type bindingsDiscovery struct {
	Path        string            `json:"path"`
	Directory   string            `json:"directory"`
	Env         map[string]string `json:"env"`
	SearchPath  []string          `json:"search_path"`
	InstalledBy string            `json:"installed_by"`
}

type bindingsFixtures struct {
	File  string             `json:"file"`
	Does  string             `json:"does"`
	Shape binding.FixtureSet `json:"shape"`
}

type bindingsReference struct {
	ID          string   `json:"id"`
	Role        string   `json:"role"`
	Directory   string   `json:"directory"`
	Credentials []string `json:"credentials,omitempty"`
	Does        string   `json:"does"`
	BindingYAML string   `json:"binding_yaml"`
	Conformance string   `json:"conformance_json,omitempty"`
}

type bindingsRegistry struct {
	Status string      `json:"status"`
	Verbs  []agentVerb `json:"verbs"`
}

const bindingsNote = "A binding is a declarative adapter (SPEC §10a): one binding.yaml, validated against `schema` below, plus fixtures/conformance.json beside it, interpreted by the engine — data, not code; it cannot execute anything and no expression language exists inside it. It declares the same manifest surface as a process adapter (id, version, role, entity_type, needs/provides, config_schema, credentials, freshness_days), so `gtme plan` treats both tiers identically. Binding roles are source (pagination + cursor), enrich (one request per record) and deliver (idempotency + dry-run receipts). The moment a binding needs logic — conditionals, multi-call workflows, OAuth dances, request signing, computation — it graduates to a process adapter: a directory holding manifest.json + an executable named `run` speaking the §5 NDJSON protocol (`gtme help --agent` documents that surface). To add an adapter: write the YAML against `schema`, model it on `reference`, record one real response per request as fixtures, place the directory on `discovery.path`, and `gtme plan` resolves `use: <id>` like a built-in."

// bindingsVerbs are the verbs that touch a binding today; kept next to
// agentVerbs so the two tables stay a visible diff apart.
var bindingsVerbs = []agentVerb{
	{"gtme plan pipeline.yaml", "resolve every `use: <id>` — built-ins first, then the discovery path; a binding that fails the schema, declares a different id than its directory, or lacks a credential is reported here, with no network and no spend"},
	{"gtme run pipeline.yaml --simulate", "execute offline: the binding's requests are answered from fixtures/conformance.json instead of the network; a binding without fixtures is surfaced as a gap"},
	{"gtme run pipeline.yaml --dry-run", "arm everything but delivery: a deliver binding receipts its resolved request instead of sending"},
	{"gtme freeze RUN_ID|last --bundle DIR", "pack the referenced bindings with their fixtures into a portable campaign bundle (ADR-029); a bundle resolves its own copies first"},
	{"gtme help --bindings", "print this document"},
}

// registryVerbs are ADR-042's registry verbs (SPEC §8 `gtme adapters`),
// listed so an author knows how a finished binding is shared and installed;
// they ship in M19 and are flagged as such until then.
var registryVerbs = []agentVerb{
	{"gtme adapters", "list installed adapters with their source and pin (.source.json)"},
	{"gtme adapters search TEXT", "search the registry index by id, vendor, description and role (GTME_REGISTRY overrides the index URL)"},
	{"gtme adapters add github.com/<owner>/<repo>/<path>[@ref]", "fetch a binding over HTTPS at a pinned ref, verify it, install it under ~/.gtme/adapters/<id, slashes → dashes>/ with .source.json beside it"},
	{"gtme adapters verify ID", "validate against the schema, run the fixtures offline, print the hosts it will call and the credentials it will demand; a binding with no fixtures, or failing ones, does not install"},
	{"gtme adapters update ID [@ref]", "re-fetch at a newer ref, only when asked"},
}

func bindingsSurface() (bindingsDoc, error) {
	ref, err := referenceBinding()
	if err != nil {
		return bindingsDoc{}, err
	}
	return bindingsDoc{
		Note: bindingsNote,
		Discovery: bindingsDiscovery{
			Path:      "~/.gtme/adapters/<directory>/binding.yaml",
			Directory: "the binding's id with slashes replaced by dashes (hubspot/contact-search → hubspot-contact-search); a nested <vendor>/<op>/ directory is also accepted. The id inside the file must match.",
			Env: map[string]string{
				"GTME_ADAPTER_PATH": "colon-separated directories searched before ~/.gtme/adapters",
				"GTME_HOME":         "replaces ~/.gtme as the home (its adapters/ subdirectory is searched)",
			},
			SearchPath:  adapters.SearchPath(),
			InstalledBy: "by hand (write the files), or by `gtme adapters add` from the registry (ADR-042)",
		},
		Fixtures: bindingsFixtures{
			File: binding.FixtureFile,
			Does: "canned HTTP responses, each matched by a substring of \"METHOD path\" (or of the full URL); the first match answers. `gtme run --simulate` serves them in place of the network, the conformance kit proves fixture payloads in → canonical records out, and `gtme adapters verify` runs them before a binding installs. Record one real response per request the binding makes; a binding without fixtures cannot be simulated, verified, or listed in the registry.",
			Shape: binding.FixtureSet{Responses: []binding.FixtureResponse{{
				Match:  "GET /v1/things",
				Status: 200,
				Body:   map[string]any{"results": []any{map[string]any{"id": "…", "email": "…"}}},
			}}},
		},
		Reference: ref,
		Verbs:     bindingsVerbs,
		Registry: bindingsRegistry{
			Status: "queued (SPEC §11 M19) — these verbs are not in this build; until then a binding is installed by hand on discovery.path",
			Verbs:  registryVerbs,
		},
	}, nil
}

// referenceBinding picks the smallest shipped binding (ADR-041: "the smallest
// shipped one, verbatim") so the worked example is the least an author needs
// to read, and it never drifts from what the binary actually validates.
func referenceBinding() (bindingsReference, error) {
	var (
		bestName string
		bestRaw  []byte
		bestFix  []byte
	)
	for _, name := range binding.Shipped() {
		sub, err := binding.ShippedFS(name)
		if err != nil {
			return bindingsReference{}, err
		}
		raw, err := fs.ReadFile(sub, "binding.yaml")
		if err != nil {
			return bindingsReference{}, fmt.Errorf("embedded %s: %w", name, err)
		}
		if bestRaw != nil && len(raw) >= len(bestRaw) {
			continue
		}
		bestName, bestRaw = name, raw
		bestFix, _ = fs.ReadFile(sub, binding.FixtureFile)
	}
	if bestRaw == nil {
		return bindingsReference{}, fmt.Errorf("no shipped bindings embedded")
	}
	b, err := binding.Parse(bestRaw)
	if err != nil {
		return bindingsReference{}, fmt.Errorf("embedded %s: %w", bestName, err)
	}
	m, err := b.Manifest()
	if err != nil {
		return bindingsReference{}, fmt.Errorf("embedded %s: %w", bestName, err)
	}
	return bindingsReference{
		ID:          m.ID,
		Role:        m.Role,
		Directory:   strings.ReplaceAll(m.ID, "/", "-"),
		Credentials: m.Credentials,
		Does:        fmt.Sprintf("the smallest binding this binary ships, verbatim — a %s binding; its fixtures file follows. Installed on discovery.path under `%s/`, it resolves exactly as the embedded copy does.", m.Role, strings.ReplaceAll(m.ID, "/", "-")),
		BindingYAML: string(bestRaw),
		Conformance: string(bestFix),
	}, nil
}
