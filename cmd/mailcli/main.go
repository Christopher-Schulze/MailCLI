package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mailcli/internal/cli"
	"mailcli/internal/mail"
	"mailcli/internal/mailapp"
	"mailcli/internal/mailstore"
)

func main() {
	os.Exit(run())
}

func run() int {
	args := os.Args[1:]
	ctx := context.Background()
	stopSignals := func() {}
	if cli.RequiresSignalContext(args) {
		ctx, stopSignals = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	}
	defer stopSignals()
	if !cli.RequiresMailService(args) {
		return cli.Run(ctx, mail.NewService(nil), args, os.Stdout, os.Stderr)
	}
	config, err := mailstore.DefaultConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	bridge := mailapp.NewClient()
	storeCtx, cancelStoreOpen := context.WithTimeout(ctx, 15*time.Second)
	client := mailstore.NewClient(storeCtx, bridge, config)
	cancelStoreOpen()
	mailService := mail.NewService(client)
	code := cli.Run(ctx, mailService, args, os.Stdout, os.Stderr)
	if err := client.Close(); err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, "close Mail store:", err)
		code = 1
	}
	return code
}
