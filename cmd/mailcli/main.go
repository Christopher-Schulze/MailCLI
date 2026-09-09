package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	return runWithConfigFactory(newFallback, newTransport, mailstore.DefaultConfig)
}

func runWithConfigFactory(
	newFallback func() mail.FallbackGateway,
	newTransport func() *invocationTransport,
	newConfig func() (mailstore.Config, error),
) int {
	args := os.Args[1:]
	jsonOutput := cli.JSONOutputRequested(args)
	ctx := context.Background()
	stopSignals := func() {}
	if cli.RequiresSignalContext(args) {
		ctx, stopSignals = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	}
	code := 0
	if !cli.RequiresMailService(args) {
		transport := newTransport()
		code = runInvocation(ctx, mail.NewServiceWithTransport(nil, "", transport.SendTransport), args, jsonOutput, transport, nil)
	} else {
		config, err := newConfig()
		if err != nil {
			if jsonOutput {
				normalized, _ := cli.NormalizeGlobalJSON(args)
				cli.WriteFailureEnvelope(os.Stdout, cli.AttemptedCommand(normalized), cli.InitializationErrorCode(err), err.Error())
				code = 1
			} else {
				fmt.Fprintln(os.Stderr, err)
				code = 1
			}
		} else {
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
			code = runInvocation(ctx, mailService, args, jsonOutput, transport, client)
		}
	}
	stopSignals()
	return code
}

func runInvocation(
	ctx context.Context,
	mailService *mail.Service,
	args []string,
	jsonOutput bool,
	transport *invocationTransport,
	store *mailstore.Client,
) int {
	var stdout bytes.Buffer
	output := io.Writer(os.Stdout)
	if jsonOutput {
		output = &stdout
	}
	code := cli.Run(ctx, mailService, args, output, os.Stderr)
	cleanupErr := transport.Close()
	if store != nil {
		cleanupErr = errors.Join(cleanupErr, store.Close())
	}
	if jsonOutput {
		return cli.FinalizeJSON(os.Stdout, args, stdout.Bytes(), code, cleanupErr)
	}
	if cleanupErr != nil {
		label := "close Mail transport:"
		if store != nil {
			label = "close Mail resources:"
		}
		fmt.Fprintln(os.Stderr, label, cleanupErr)
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
