package main

import (
	"context"
	"fmt"
	"os"

	"mailcli/internal/cli"
	"mailcli/internal/mail"
	"mailcli/internal/mailapp"
	"mailcli/internal/mailstore"
)

func main() {
	ctx := context.Background()
	config, err := mailstore.DefaultConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bridge := mailapp.NewClient()
	client := mailstore.NewClient(ctx, bridge, config)
	mailService := mail.NewService(client)
	code := cli.Run(ctx, mailService, os.Args[1:], os.Stdout, os.Stderr)
	if err := client.Close(); err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, "close Mail store:", err)
		code = 1
	}
	os.Exit(code)
}
