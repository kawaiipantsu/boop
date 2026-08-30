package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/stats"
	"github.com/kawaiipantsu/boop/web"
)

// runWebUI implements --web: the local WebUI over the same runtime the CLI and
// TUI use (§2.3, §22).
//
// Approvals are served by a permissions.Broker rather than the terminal
// approver, so a confirmation raised by a model reaches the browser instead of
// blocking on a prompt nobody is watching (§50).
func runWebUI(ctx context.Context, opts options, stdout, stderr io.Writer) error {
	cfg, err := loadConfig(opts)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	broker := permissions.NewBroker(permissions.WithContext(ctx))
	defer broker.Close()

	application, err := app.New(ctx, app.Options{
		Config:   cfg,
		Approver: broker,
		Stderr:   stderr,
		Verbose:  opts.verbose,
	})
	if err != nil {
		return err
	}
	defer func() { _ = application.Close() }()

	server, err := web.New(web.Options{
		App:               application,
		Config:            cfg,
		Broker:            broker,
		Stats:             stats.New(),
		Listen:            opts.listen,
		Port:              opts.port,
		AllowInsecureBind: opts.allowInsecureBind,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "boop web ui: http://%s\n", server.Addr())
	fmt.Fprintln(stderr, "press ctrl-c to stop")

	if err := server.Run(ctx); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "boop: web ui stopped")
	return nil
}
