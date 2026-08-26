package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"mailcli/internal/cli"
	"mailcli/internal/mail"
	"mailcli/internal/mailapp"
	"mailcli/internal/mailstore"
)

func main() {
	ctx := context.Background()
	args := os.Args[1:]
	if !cli.RequiresMailService(args) {
		os.Exit(cli.Run(ctx, nil, args, os.Stdout, os.Stderr))
	}
	config, err := mailstore.DefaultConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
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
	os.Exit(code)
}
