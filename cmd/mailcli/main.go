package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"mailcli/internal/cli"
	"mailcli/internal/keychain"
	"mailcli/internal/mail"
	"mailcli/internal/mailapp"
	"mailcli/internal/mailstore"
	"mailcli/internal/transport/imapclient"
	"mailcli/internal/transport/smtpclient"
)

func main() {
	if cli.RequiresMainThread(os.Args[1:]) {
		runtime.LockOSThread()
	}
	os.Exit(run())
}

func run() int {
	return runWithFallbackFactory(func() mail.FallbackGateway {
		return mailapp.NewClient()
	})
}

func runWithFallbackFactory(newFallback func() mail.FallbackGateway) int {
	args := os.Args[1:]
	ctx := context.Background()
	stopSignals := func() {}
	if cli.RequiresSignalContext(args) {
		ctx, stopSignals = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	}
	defer stopSignals()
	if !cli.RequiresMailService(args) {
		return cli.Run(ctx, mail.NewServiceWithTransport(nil, "", sendTransport()), args, os.Stdout, os.Stderr)
	}
	config, err := mailstore.DefaultConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var fallback mail.FallbackGateway
	if newFallback != nil {
		fallback = newFallback()
	}
	storeCtx, cancelStoreOpen := context.WithTimeout(ctx, 15*time.Second)
	transport := sendTransport()
	config.AccountBindings = transport.AccountBindings
	client := mailstore.NewClient(storeCtx, fallback, config, transport)
	cancelStoreOpen()
	mailService := mail.NewServiceWithTransport(client, "", transport)
	code := cli.Run(ctx, mailService, args, os.Stdout, os.Stderr)
	if err := client.Close(); err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, "close Mail store:", err)
		code = 1
	}
	return code
}

// sendTransport builds the direct SMTP/IMAP send transport with keychain
// credentials. It performs no I/O until a send actually runs.
func sendTransport() mail.SendTransport {
	imap := imapclient.New()
	return mail.SendTransport{
		Submitter:       smtpclient.New(),
		Mirror:          imap,
		Credentials:     keychain.New(),
		AccountBindings: mail.DefaultAccountBindingStore(),
		Imap:            imap,
	}
}
