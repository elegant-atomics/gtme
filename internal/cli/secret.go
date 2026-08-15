package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/trevorfox/gtm/internal/secrets"
	"golang.org/x/term"
)

// cmdSecret manages ~/.gtm/secrets (SPEC §8). Values are never echoed and never
// printed back.
func cmdSecret(ctx context.Context, env Env, args []string) error {
	fs := flag.NewFlagSet("secret", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return fail(ExitValidation, "usage: gtm secret set KEY [VALUE] | gtm secret list")
	}

	switch positional[0] {
	case "set":
		if len(positional) < 2 {
			return fail(ExitValidation, "usage: gtm secret set KEY [VALUE]")
		}
		key := positional[1]
		var value string
		switch {
		case len(positional) >= 3:
			value = strings.Join(positional[2:], " ")
		default:
			value, err = readSecret(env, key)
			if err != nil {
				return err
			}
		}
		if strings.TrimSpace(value) == "" {
			return fail(ExitValidation, "%s: empty value", key)
		}
		if err := secrets.Set(key, value); err != nil {
			return fail(ExitOther, "%v", err)
		}
		path, _ := secrets.Path()
		fmt.Fprintf(env.Stderr, "stored %s in %s\n", key, path)
		return nil

	case "list":
		names, err := secrets.Names()
		if err != nil {
			return fail(ExitOther, "%v", err)
		}
		if len(names) == 0 {
			fmt.Fprintln(env.Stderr, "no secrets stored — add one with `gtm secret set KEY`")
			return nil
		}
		for _, name := range names {
			// Names only. A secrets manager that prints values is not one.
			fmt.Fprintln(env.Stderr, name)
		}
		return nil

	default:
		return fail(ExitValidation, "unknown secret command %q (want set or list)", positional[0])
	}
}

// readSecret reads a value without echoing it when stdin is a terminal, and
// straight from stdin when it is piped (`echo "$KEY" | gtm secret set NAME`).
func readSecret(env Env, key string) (string, error) {
	if f, ok := env.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprintf(env.Stderr, "%s: ", key)
		raw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(env.Stderr)
		if err != nil {
			return "", fail(ExitOther, "reading %s: %v", key, err)
		}
		return string(raw), nil
	}

	sc := bufio.NewScanner(env.Stdin)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", fail(ExitOther, "reading %s: %v", key, err)
		}
		return "", fail(ExitValidation, "%s: no value on stdin", key)
	}
	return strings.TrimSpace(sc.Text()), nil
}
