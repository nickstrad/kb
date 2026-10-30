// Command kb is the knowledge-store CLI; the command tree lives in internal/cli.
//
// It must be built with the fts5 build tag, because the SQLite driver only compiles FTS5 in under
// that tag:
//
//	go build -tags fts5 ./cmd/kb
//
// Release builds stamp version with -ldflags "-X main.version=<tag>". Otherwise kb reports the
// version and VCS revision Go records in the build info (see cli.ResolveVersion).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/nickstrad/kb/internal/cli"
)

var version = "dev"

func main() {
	cli.Version = cli.ResolveVersion(version)
	// Ctrl-C and SIGTERM cancel the context every command handler reads through cmd.Context(),
	// so an in-flight embedding call or query stops instead of being killed mid-transaction.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop() // not deferred: os.Exit does not run deferred functions.
	os.Exit(code)
}
