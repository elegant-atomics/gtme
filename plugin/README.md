# gtme plugin for Claude Code

Four skills that turn a goal into a campaign pipeline, walk it up the
ladder from plan to armed, add a vendor gtme does not ship, and read what
the ledger knows. Each skill is a procedure and the rules; every fact comes
from the binary at run time (`gtme help --agent`, `gtme help --bindings`),
from `gtme.run/start.md`, or from the pattern bundles in `bundles/`, so the
skills cannot drift from the version installed.

```
/plugin marketplace add gtme-run/gtme
/plugin install gtme@gtme-run
```

| Skill | Fires when |
|---|---|
| `/gtme:create-pipeline` | someone describes an outreach goal and wants it built |
| `/gtme:run-pipeline` | a pipeline or bundle exists and needs to run, rehearse, or ship |
| `/gtme:create-adapter` | a vendor or data source gtme lacks, or records that are not people or companies |
| `/gtme:analyze` | what happened, what it cost, why a record stopped, what is known about someone |

Two rules every skill carries, from `START.md`:

- **Never arm a live target unasked.** A `gtme run` with no `--simulate`
  or `--dry-run` on a pipeline whose deliver step reaches a vendor is run
  after the human has read the dry-run receipt and said so. Local targets
  (a CSV, a group) are the agent's to run.
- **Never handle a key.** `gtme secret set KEY` prompts the human. No key
  goes on a command line, into a file, or into the chat.

Install gtme itself: `brew install gtme-run/tap/gtme`, or see
[gtme.run/get](https://gtme.run/get). First run on a machine:
[gtme.run/start.md](https://gtme.run/start.md), door 1.

The plugin's version tracks the binary's tag; both bump in the same
commit. `test/e2e/plugin_test.go` runs every command block in the skills
against the built binary.
