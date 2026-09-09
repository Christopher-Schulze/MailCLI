package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync"
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
	return runWithFactories(newFallback, newInvocationTransport)
}

func runWithFactories(
	newFallback func() mail.FallbackGateway,
	newTransport func() *invocationTransport,
) int {
	args := os.Args[1:]
	ctx := context.Background()
	stopSignals := func() {}
	if cli.RequiresSignalContext(args) {
		ctx, stopSignals = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	}
	defer stopSignals()
	if !cli.RequiresMailService(args) {
		transport := newTransport()
		code := cli.Run(ctx, mail.NewServiceWithTransport(nil, "", transport.SendTransport), args, os.Stdout, os.Stderr)
		if closeErr := transport.Close(); closeErr != nil {
			fmt.Fprintln(os.Stderr, "close Mail transport:", closeErr)
			if code == 0 {
				code = 1
			}
		}
		return code
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
	transport := newTransport()
	config.AccountBindings = transport.AccountBindings
	client := mailstore.NewClient(storeCtx, fallback, config, transport.SendTransport)
	cancelStoreOpen()
	mailService := mail.NewServiceWithTransport(client, "", transport.SendTransport)
	code := cli.Run(ctx, mailService, args, os.Stdout, os.Stderr)
	cleanupErr := closeInvocationResources(transport.Close, client.Close)
	if cleanupErr != nil {
		fmt.Fprintln(os.Stderr, "close Mail resources:", cleanupErr)
		if code == 0 {
			code = 1
		}
	}
	return code
}

type invocationTransport struct {
	mail.SendTransport
	closeOnce     sync.Once
	closeResource func() error
	closeErr      error
}

func (t *invocationTransport) Close() error {
	t.closeOnce.Do(func() {
		if t.closeResource != nil {
			t.closeErr = t.closeResource()
		}
	})
	return t.closeErr
}

func closeInvocationResources(closeTransport, closeStore func() error) error {
	transportErr := closeTransport()
	storeErr := closeStore()
	return errors.Join(transportErr, storeErr)
}

// newInvocationTransport builds the direct SMTP/IMAP send transport with
// keychain credentials and owns its IMAP pool for one invocation. It performs
// no I/O until a send actually runs.
func newInvocationTransport() *invocationTransport {
	imap := imapclient.New()
	return &invocationTransport{
		SendTransport: mail.SendTransport{
			Submitter:       smtpclient.New(),
			Mirror:          imap,
			Credentials:     keychain.New(),
			AccountBindings: mail.DefaultAccountBindingStore(),
			Imap:            imap,
		},
		closeResource: imap.Close,
	}
}
