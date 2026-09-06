package cli

// gtme groups — the ADR-021 verb set (SPEC §8): list groups with their derived
// character, inspect one, and hand-edit membership by key or by snapshotting
// an intensional definition (--from-segment / --query) into extensional
// membership. Everything is events; membership edits are append-only.

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/elegant-atomics/gtme/internal/ledger"
	"github.com/elegant-atomics/gtme/internal/registry"
)

func cmdGroups(ctx context.Context, env Env, args []string) error {
	if len(args) == 0 {
		return groupsList(ctx, env)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "show":
		if len(rest) != 1 {
			return fail(ExitValidation, "usage: gtme groups show NAME")
		}
		return groupsShow(ctx, env, rest[0])
	case "add":
		return groupsEdit(ctx, env, rest, ledger.GroupAdded)
	case "remove":
		return groupsEdit(ctx, env, rest, ledger.GroupRemoved)
	default:
		return fail(ExitValidation,
			"usage: gtme groups [show NAME | add NAME KEY...|--from-segment NAME|--query SQL|--type TYPE | remove NAME KEY... [--note TEXT]]")
	}
}

func groupsList(ctx context.Context, env Env) error {
	l, err := openLedger(ctx)
	if err != nil {
		return err
	}
	defer l.Close()
	groups, err := l.Groups(ctx)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		fmt.Fprintln(env.Stderr, "no groups yet — `gtme groups add NAME KEY...`, a pipeline terminus (group:), or a deliver record: creates one")
		return nil
	}
	tw := tabwriter.NewWriter(env.Stderr, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "group\ttype\tmembers\tadded\tremoved\ttouched\tcreated")
	for _, g := range groups {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%s\n",
			g.Name, groupType(g.EntityType), g.Members, g.Added, g.Removed, g.Touched, g.CreatedAt.Format("2006-01-02"))
	}
	return tw.Flush()
}

func groupsShow(ctx context.Context, env Env, name string) error {
	l, err := openLedger(ctx)
	if err != nil {
		return err
	}
	defer l.Close()
	g, err := l.GetGroup(ctx, name)
	if err != nil {
		return fail(ExitValidation, "%v", err)
	}
	members, err := l.GroupMembers(ctx, g.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "group %s (%s) — %d member(s)\n", g.Name, groupType(g.EntityType), len(members))
	for _, m := range members {
		fmt.Fprintf(env.Stderr, "  %s:%s\n", m.EntityType, m.IdentityKey)
	}
	// The chain the group sits in (SPEC §8, ADR-054): derived from
	// group_events.run_id and runs, never stored.
	producers, err := l.GroupProducers(ctx, g.ID)
	if err != nil {
		return err
	}
	consumers, err := l.GroupConsumers(ctx, g.Name)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "written by:  %s\n", orNone(producers))
	fmt.Fprintf(env.Stderr, "sourced by:  %s\n", orNone(consumers))
	events, err := l.GroupEvents(ctx, g.ID, 10)
	if err != nil {
		return err
	}
	if len(events) > 0 {
		fmt.Fprintln(env.Stderr, "recent events:")
		for _, ev := range events {
			line := fmt.Sprintf("  %s  %-8s %s", ev.CreatedAt.Format("2006-01-02 15:04"), ev.Event, ev.IdentityKey)
			if ev.Detail != "" {
				line += "  " + ev.Detail
			}
			fmt.Fprintln(env.Stderr, line)
		}
	}
	return nil
}

// groupsEdit applies add/remove events from keys, a saved segment, or a
// one-off query (SPEC §8).
func groupsEdit(ctx context.Context, env Env, args []string, event string) error {
	fs := flag.NewFlagSet("groups "+event, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fromSegment := fs.String("from-segment", "", "snapshot a saved segment's identity_id column into membership")
	query := fs.String("query", "", "snapshot a read-only SELECT's identity_id column into membership")
	entityType := fs.String("type", "", "the group's entity type (ADR-054): required when a key matches more than one type or no key is given; sets an untyped group's type once")
	note := fs.String("note", "", "remove only: the reason, recorded in the event's detail (ADR-032)")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		if event == ledger.GroupRemoved {
			return fail(ExitValidation, "usage: gtme groups remove NAME KEY... [--note TEXT]")
		}
		return fail(ExitValidation, "usage: gtme groups add NAME [KEY...] [--from-segment NAME | --query SQL] [--type TYPE]")
	}
	name, keys := positional[0], positional[1:]
	if event == ledger.GroupRemoved && (*fromSegment != "" || *query != "") {
		return fail(ExitValidation, "snapshots only add; remove takes identity keys")
	}
	if event == ledger.GroupAdded && *note != "" {
		return fail(ExitValidation, "--note records why a record was removed; add has no reason to record (SPEC §8)")
	}
	typed := strings.TrimSpace(*entityType)
	if len(keys) == 0 && *fromSegment == "" && *query == "" && !(event == ledger.GroupAdded && typed != "") {
		return fail(ExitValidation, "nothing to %s: give identity keys, --from-segment, --query, or (add) --type", verbFor(event))
	}
	if typed != "" {
		reg, err := registry.Load()
		if err != nil {
			return err
		}
		if _, err := reg.Resolve(typed); err != nil {
			return fail(ExitValidation, "--type: %v", err)
		}
	}

	l, err := openLedger(ctx)
	if err != nil {
		return err
	}
	defer l.Close()

	// Resolve the members first: a new group takes its type from them when
	// no --type says (SPEC §8, ADR-054) — an unambiguous key match, or the
	// query's identities.
	var ids []string
	detail := map[string]any{"via": "cli"}
	if strings.TrimSpace(*note) != "" {
		detail["note"] = strings.TrimSpace(*note)
	}
	switch {
	case *fromSegment != "":
		saved, err := l.SavedQuery(ctx, *fromSegment)
		if err != nil {
			return fail(ExitValidation, "segment %q: %v", *fromSegment, err)
		}
		if ids, err = l.IdentityIDsFromSQL(ctx, saved.SQL); err != nil {
			return fail(ExitValidation, "%v", err)
		}
		detail = map[string]any{"segment": *fromSegment, "evaluated_at": nowStamp()}
	case *query != "":
		if ids, err = l.IdentityIDsFromSQL(ctx, *query); err != nil {
			return fail(ExitValidation, "%v", err)
		}
		detail = map[string]any{"query": *query, "evaluated_at": nowStamp()}
	}
	for _, key := range keys {
		ident, err := lookupIdentity(ctx, l, key, typed)
		if err != nil {
			return fail(ExitValidation, "%v", err)
		}
		ids = append(ids, ident.ID)
	}
	memberType, err := identityTypeOf(ctx, l, ids)
	if err != nil {
		return fail(ExitValidation, "%v", err)
	}
	if typed != "" && memberType != "" && memberType != typed {
		return fail(ExitValidation, "--type %s, but the records to add are %s", typed, memberType)
	}

	var g ledger.Group
	if event == ledger.GroupAdded {
		groupType := typed
		if groupType == "" {
			groupType = memberType
		}
		if g, err = l.EnsureGroup(ctx, name, groupType); err != nil {
			return fail(ExitValidation, "%v", err)
		}
		if g.EntityType == "" && groupType != "" {
			// A group created before ADR-054: --type (or the first typed
			// members after it, only when --type says so) sets it once.
			if typed != "" {
				if err := l.SetGroupType(ctx, g.ID, typed); err != nil {
					return err
				}
				g.EntityType = typed
				fmt.Fprintf(env.Stderr, "group %s: type set to %s\n", g.Name, typed)
			}
		}
	} else if g, err = l.GetGroup(ctx, name); err != nil {
		return fail(ExitValidation, "%v", err)
	}
	if g.EntityType != "" && memberType != "" && memberType != g.EntityType {
		return fail(ExitValidation, "group %s holds %s records; a %s cannot join it (SPEC §8, ADR-054)", g.Name, g.EntityType, memberType)
	}
	members, err := l.GroupMembership(ctx, g.ID)
	if err != nil {
		return err
	}

	changed := 0
	for _, id := range ids {
		isMember := members[id]
		if (event == ledger.GroupAdded && isMember) || (event == ledger.GroupRemoved && !isMember) {
			continue // membership edits are idempotent; the trail stays clean
		}
		if err := l.AddGroupEvent(ctx, g.ID, id, event, detail, ""); err != nil {
			return err
		}
		members[id] = event == ledger.GroupAdded
		changed++
	}
	fmt.Fprintf(env.Stderr, "group %s: %d %s, %d unchanged\n", g.Name, changed, event, len(ids)-changed)
	return nil
}

// identityTypeOf reports the one entity type a set of identities share, ""
// for none, and an error when they mix (a group holds one type, ADR-054).
func identityTypeOf(ctx context.Context, l *ledger.Ledger, ids []string) (string, error) {
	seen := ""
	for _, id := range ids {
		ident, err := l.IdentityByID(ctx, id)
		if err != nil {
			return "", err
		}
		switch {
		case seen == "":
			seen = ident.EntityType
		case seen != ident.EntityType:
			return "", fmt.Errorf("the records to add mix %s and %s; a group holds one type (SPEC §8, ADR-054)", seen, ident.EntityType)
		}
	}
	return seen, nil
}

func groupType(t string) string {
	if t == "" {
		return "(untyped)"
	}
	return t
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "(none)"
	}
	return strings.Join(list, ", ")
}

func lookupIdentity(ctx context.Context, l *ledger.Ledger, key, entityType string) (ledger.Identity, error) {
	if entityType != "" {
		return l.IdentityByKey(ctx, entityType, key)
	}
	return l.FindByKey(ctx, key)
}

func verbFor(event string) string {
	if event == ledger.GroupRemoved {
		return "remove"
	}
	return "add"
}

func nowStamp() string {
	return time.Now().UTC().Format(ledger.TimeFormat)
}
