// Command gtm is a CLI for GTM data pipelines. See SPEC.md.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/trevorfox/gtm/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, cli.DefaultEnv()))
}
