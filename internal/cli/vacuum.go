package cli

// gtme vacuum (SPEC §8, ADR-030): evict expired payloads — and nothing else.
// Facts are append-only forever; payload eviction is the one legitimate
// deletion in the system, and it happens opportunistically at run start too.

import (
	"context"
	"fmt"
)

func cmdVacuum(ctx context.Context, env Env, args []string) error {
	if len(args) != 0 {
		return fail(ExitValidation, "usage: gtme vacuum")
	}
	l, err := openLedger(ctx)
	if err != nil {
		return err
	}
	defer l.Close()
	n, err := l.PurgeExpiredPayloads(ctx)
	if err != nil {
		return fail(ExitOther, "%v", err)
	}
	fmt.Fprintf(env.Stderr, "evicted %d expired payload(s); facts untouched\n", n)
	return nil
}
