package e2e

// The Claude Code plugin (plugin/) is instruction, not knowledge: its skills
// name gtme commands and point at `gtme help --agent` for every fact. Two
// things keep that honest across releases. Every ```sh block in a SKILL.md
// runs against the built binary in a fresh harness seeded with the files the
// skills name (pipeline.yaml, leads.csv), and every `gtme <verb>` a skill
// mentions is a verb `gtme help --agent` lists. A renamed flag or a retired
// verb fails the build here, not in someone's session.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// skillPipeline is the pipeline the skills' examples assume exists as
// pipeline.yaml: a local CSV in, a deterministic judgment, a local CSV out —
// armable with zero keys, so a block that runs it armed spends nothing.
const skillPipeline = `name: pipeline
version: 1
source:
  use: csv/source
  with:
    path: leads.csv
    columns: { full_name: Full Name, email: Email, title: Title, company_domain: Company Website }
steps:
  - id: score
    use: demo/enrich
    with:
      cost_per_record_usd: 0.01
  - id: fit
    use: sql/filter
    with:
      query: >
        SELECT identity_id FROM current_values
        WHERE field = 'demo.score' AND CAST(value AS INTEGER) >= 50
  - id: out
    use: csv/deliver
    with:
      path: out.csv
    variables:
      name: full_name
      score: demo.score
    idempotency: email
group: qualified
`

const skillLeads = "Full Name,Email,Title,Company Website\n" +
	"Jane Doe,jane.doe@acme.com,VP Marketing,https://www.acme.com\n" +
	"Bob Stone,bob@globex.io,Head of Growth,globex.io\n" +
	"Carol Ray,carol@initech.dev,Software Engineer,initech.dev\n"

var (
	fencedSh  = regexp.MustCompile("(?s)```sh\n(.*?)```")
	gtmeVerb  = regexp.MustCompile("(?m)(?:^|`)gtme ([a-z][a-z-]*)") // code only: a line of a block, or an inline span
	skipBlock = regexp.MustCompile(`<[a-z][a-z-]*>|secret set|brew |curl |/plugin |\$\(|[|>]`)
)

func skillFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRoot(), "plugin", "skills", "*", "SKILL.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no plugin skills found: %v", err)
	}
	return files
}

// TestPluginSkillsNameRealVerbs: every `gtme <verb>` in a skill is a verb the
// binary documents (help --agent's verbs, plus the `help` family itself).
func TestPluginSkillsNameRealVerbs(t *testing.T) {
	h := newHarness(t)
	raw := h.mustRun("help", "--agent").stdout
	var doc struct {
		Verbs []struct {
			Usage string `json:"usage"`
		} `json:"verbs"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(raw, "\n", 2)[0]), &doc); err != nil {
		t.Fatalf("help --agent: %v", err)
	}
	known := map[string]bool{"help": true, "version": true}
	for _, v := range doc.Verbs {
		f := strings.Fields(v.Usage)
		if len(f) >= 2 && f[0] == "gtme" {
			known[f[1]] = true
		}
	}
	for _, file := range skillFiles(t) {
		// Only code is checked: fenced blocks and inline spans. Prose may say
		// "gtme does" without naming a verb.
		body := readFile(t, file)
		var code strings.Builder
		for _, b := range fencedSh.FindAllStringSubmatch(body, -1) {
			code.WriteString(b[1] + "\n")
		}
		for _, span := range regexp.MustCompile("`[^`\n]+`").FindAllString(body, -1) {
			code.WriteString(span + "\n")
		}
		for _, m := range gtmeVerb.FindAllStringSubmatch(code.String(), -1) {
			if !known[m[1]] {
				t.Errorf("%s names `gtme %s`, which help --agent does not list", filepath.Base(filepath.Dir(file)), m[1])
			}
		}
	}
}

// TestPluginSkillCommandBlocksRun: each ```sh block runs, line by line, in a
// fresh harness seeded with the skills' example files. Blocks with
// placeholders, key handling, installs or fetches are documentation and are
// skipped; everything else must exit 0 — except lines a skill marks as
// expected to fail with `# exit N`.
func TestPluginSkillCommandBlocksRun(t *testing.T) {
	for _, file := range skillFiles(t) {
		skill := filepath.Base(filepath.Dir(file))
		t.Run(skill, func(t *testing.T) {
			h := newHarness(t)
			h.write("pipeline.yaml", skillPipeline)
			h.write("leads.csv", skillLeads)
			ai := h.fixtureScript("auto.json", "$auto")
			// The ledger a skill's examples assume: one armed run, so
			// `runs last`, `show <key>`, `groups show qualified` all answer.
			if seed := h.runWithEnv(ai, "", "run", "pipeline.yaml"); seed.code != 0 {
				t.Fatalf("seed run exit = %d\n%s", seed.code, seed.stderr)
			}
			ran := 0
			for _, block := range fencedSh.FindAllStringSubmatch(readFile(t, file), -1) {
				if skipBlock.MatchString(block[1]) {
					continue
				}
				for _, line := range strings.Split(block[1], "\n") {
					cmd := strings.TrimSpace(line)
					if cmd == "" || strings.HasPrefix(cmd, "#") || !strings.HasPrefix(cmd, "gtme ") {
						continue
					}
					wantExit := 0
					if i := strings.Index(cmd, "# exit "); i >= 0 {
						wantExit = int(cmd[i+7] - '0')
						cmd = strings.TrimSpace(cmd[:i])
					} else if i := strings.Index(cmd, "#"); i >= 0 {
						cmd = strings.TrimSpace(cmd[:i])
					}
					args := shellFields(cmd)[1:]
					res := h.runWithEnv(ai, "", args...)
					ran++
					if res.code != wantExit {
						t.Errorf("%s: `%s` exited %d, want %d\nstderr:\n%s", skill, cmd, res.code, wantExit, res.stderr)
					}
				}
			}
			if ran == 0 {
				t.Errorf("%s: no runnable command blocks — a skill should prove at least one command", skill)
			}
		})
	}
}

// shellFields splits a command line on spaces, honouring double quotes —
// enough for the skills' `gtme query "SELECT ..."` examples.
func shellFields(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// unused guard so the file compiles before any skill exists.
var _ = os.ReadFile
