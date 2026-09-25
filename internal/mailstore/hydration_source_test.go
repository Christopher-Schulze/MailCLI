package mailstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

type sourceImapOperator struct {
	*stubImapOperator
	directory    string
	files        []*os.File
	closed       int
	closeErr     error
	reportedSize *int64
	lastBound    int64
	fetchClock   *hydrationFakeClock
	fetchElapsed time.Duration
	fetchBudget  time.Duration
	stallFetch   bool
	cancelFetch  context.CancelFunc
}

type hydrationFakeClock struct{ elapsed time.Duration }

func (clock *hydrationFakeClock) Advance(duration time.Duration) {
	clock.elapsed += duration
}

func TestLocalReadContextRemainsSixtySecondsAndHonorsCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	ctx, cancel := localReadContext(parent)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("local read context has no deadline")
	}
	remaining := time.Until(deadline)
	if remaining > mail.LocalReadTimeout || mail.LocalReadTimeout-remaining > 5*time.Second {
		t.Fatalf("local read timeout = %v, want %v", remaining, mail.LocalReadTimeout)
	}
	cancelParent()
	if ctx.Err() != context.Canceled {
		t.Fatalf("local cancellation = %v, want context.Canceled", ctx.Err())
	}
}

type trackedHydrationSource struct {
	*os.File
	owner *sourceImapOperator
}

func (source *trackedHydrationSource) Close() error {
	source.owner.closed++
	return errors.Join(source.File.Close(), source.owner.closeErr)
}

func (operator *sourceImapOperator) FetchMessageReader(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid uint32, validity uint32, maximum int64) (io.ReadSeekCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if operator.cancelFetch != nil {
		operator.cancelFetch()
		operator.cancelFetch = nil
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if operator.fetchClock != nil {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, 0, errors.New("hydration FETCH context has no deadline")
		}
		operator.fetchBudget = time.Until(deadline)
		if operator.stallFetch || operator.fetchElapsed >= operator.fetchBudget {
			operator.fetchClock.Advance(operator.fetchBudget)
			return nil, 0, context.DeadlineExceeded
		}
		operator.fetchClock.Advance(operator.fetchElapsed)
	}
	operator.lastBound = maximum
	file, err := os.CreateTemp(operator.directory, "source-*")
	if err != nil {
		return nil, 0, err
	}
	if _, err := file.Write(operator.raw); err != nil {
		return nil, 0, errors.Join(err, file.Close())
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, 0, errors.Join(err, file.Close())
	}
	operator.files = append(operator.files, file)
	size := int64(len(operator.raw))
	if operator.reportedSize != nil {
		size = *operator.reportedSize
		if size > int64(len(operator.raw)) {
			if err := file.Truncate(size); err != nil {
				return nil, 0, errors.Join(err, file.Close())
			}
		}
	}
	return &trackedHydrationSource{File: file, owner: operator}, size, nil
}

func TestHydrationFetchCompletesAfterLocalReadDeadline(t *testing.T) {
	client, ref, operator := hydrationReaderFixture(t, "Subject: bounded fixture\r\n\r\nbody")
	size := mail.MaximumRawSourceBytes
	operator.reportedSize = &size
	operator.fetchClock = &hydrationFakeClock{}
	operator.fetchElapsed = 61 * time.Second
	if operator.fetchElapsed <= mail.LocalReadTimeout {
		t.Fatalf("simulated FETCH duration = %v, must exceed local read timeout %v", operator.fetchElapsed, mail.LocalReadTimeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), mail.LocalReadTimeout+transport.TransferBudgetCap)
	defer cancel()

	source, gotSize, _, err := client.hydrateMessageSource(ctx, ref, false)
	if err != nil {
		t.Fatalf("hydrateMessageSource() error = %v", err)
	}
	end, seekErr := source.Seek(0, io.SeekEnd)
	if seekErr != nil || end != size {
		_ = source.Close()
		t.Fatalf("hydrated source length = %d, error %v, want %d", end, seekErr, size)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close hydrated source: %v", err)
	}
	if gotSize != size || operator.lastBound != mail.MaximumRawSourceBytes {
		t.Fatalf("hydrated size/bound = %d/%d, want %d/%d", gotSize, operator.lastBound, size, mail.MaximumRawSourceBytes)
	}
	if operator.fetchBudget > transport.TransferBudgetForSize(size) ||
		transport.TransferBudgetForSize(size)-operator.fetchBudget > 5*time.Second {
		t.Fatalf("simulated FETCH budget = %v, want %v", operator.fetchBudget, transport.TransferBudgetForSize(size))
	}
	if operator.fetchClock.elapsed != operator.fetchElapsed {
		t.Fatalf("fake clock elapsed = %v, want %v", operator.fetchClock.elapsed, operator.fetchElapsed)
	}
}

func TestStalledHydrationFetchEndsAtComputedBudget(t *testing.T) {
	client, ref, operator := hydrationReaderFixture(t, "Subject: bounded fixture\r\n\r\nbody")
	operator.fetchClock = &hydrationFakeClock{}
	operator.stallFetch = true

	_, _, _, err := client.hydrateMessageSource(context.Background(), ref, false)
	wantBudget := transport.TransferBudgetForSize(mail.MaximumRawSourceBytes)
	if !errors.Is(err, context.DeadlineExceeded) || transport.ErrorCode(err) != operationTimeoutCode {
		t.Fatalf("stalled hydration error = %v (code %q), want bounded timeout", err, transport.ErrorCode(err))
	}
	if operator.fetchBudget > wantBudget || wantBudget-operator.fetchBudget > 5*time.Second ||
		operator.fetchClock.elapsed < wantBudget-5*time.Second || operator.fetchClock.elapsed > wantBudget {
		t.Fatalf("stalled FETCH budget/elapsed = %v/%v, want %v", operator.fetchBudget, operator.fetchClock.elapsed, wantBudget)
	}
	if !strings.Contains(err.Error(), "no external mutation was attempted") {
		t.Fatalf("timeout error lacks read-only effect: %v", err)
	}
}

func TestHydrationFetchHonorsCallerCancellation(t *testing.T) {
	client, ref, operator := hydrationReaderFixture(t, "Subject: bounded fixture\r\n\r\nbody")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	operator.cancelFetch = cancel

	message, err := client.GetMessage(ctx, ref)
	if !errors.Is(err, context.Canceled) || transport.ErrorCode(err) != operationCanceledCode || ctx.Err() != context.Canceled {
		t.Fatalf("canceled hydration = %+v, error %v", message, err)
	}
	if !strings.Contains(err.Error(), "no external mutation was attempted") {
		t.Fatalf("canceled hydration error lacks read-only effect: %v", err)
	}
}

func hydrationReaderFixture(t *testing.T, raw string) (*Client, string, *sourceImapOperator) {
	t.Helper()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "identity@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	ref := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	ref = messageRefWithExpectedID(t, ref, "<101@example.com>")
	location, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.messageBasePath(location, 101)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(base + ".emlx"); err != nil {
		t.Fatal(err)
	}
	operator := &sourceImapOperator{stubImapOperator: &stubImapOperator{raw: []byte(raw), boxes: []transport.MailboxInfo{{Name: "INBOX"}}, uid: 101}, directory: t.TempDir()}
	client := &Client{store: store, send: mail.SendTransport{Imap: operator, Credentials: stubCredentials{"identity@gmail.com": "secret"}}}
	return client, ref, operator
}

func TestHydrationConsumersUseOwnedReaders(t *testing.T) {
	body := strings.Repeat("complete body ", 100000)
	raw := "Message-ID: <101@example.com>\r\nFrom: Alice <alice@example.com>\r\nSubject: Quarterly Report\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n" +
		"--b\r\nContent-Disposition: attachment; filename=invoice.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\naW52b2ljZS1ieXRlcw==\r\n--b--\r\n"
	client, ref, operator := hydrationReaderFixture(t, raw)
	for _, operation := range []string{"message", "raw", "write", "attachment"} {
		t.Run(operation, func(t *testing.T) {
			switch operation {
			case "message":
				message, err := client.GetMessage(context.Background(), ref)
				if err != nil || message.Content != strings.TrimSpace(body) || !message.ContentComplete || message.ContentSource != "imap_raw" || len(message.Attachments) != 1 || message.Summary.Ref == "" {
					t.Fatalf("message mismatch: error=%v complete=%v attachments=%d", err, message.ContentComplete, len(message.Attachments))
				}
			case "raw":
				got, err := client.GetRawSource(context.Background(), ref)
				if err != nil || got != raw {
					t.Fatalf("raw mismatch: %v", err)
				}
			case "write":
				var output strings.Builder
				if err := client.WriteRawSource(context.Background(), ref, &output); err != nil || output.String() != raw {
					t.Fatalf("write mismatch: %v", err)
				}
			case "attachment":
				output := filepath.Join(t.TempDir(), "invoice.pdf")
				evidence, err := client.SaveAttachmentToWithEvidence(context.Background(), ref, "2", output)
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(output)
				if err != nil || string(got) != "invoice-bytes" || evidence.Size != int64(len(got)) {
					t.Fatalf("attachment mismatch: %+v %v", evidence, err)
				}
			}
			if operator.closed != len(operator.files) || operator.lastFetchMax != 0 || operator.lastBound != mail.MaximumRawSourceBytes {
				t.Fatalf("reader ownership or routing failed: closed=%d files=%d byteBound=%d readerBound=%d", operator.closed, len(operator.files), operator.lastFetchMax, operator.lastBound)
			}
			for _, file := range operator.files {
				if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("open source remains: %v", err)
				}
			}
		})
	}
}

func TestHydrationSourceRejectsInvalidSizeAndPropagatesClose(t *testing.T) {
	for _, size := range []int64{-1, mail.MaximumRawSourceBytes + 1} {
		client, ref, operator := hydrationReaderFixture(t, "Subject: fixture\r\n\r\nbody")
		operator.reportedSize = &size
		if _, err := client.GetRawSource(context.Background(), ref); err == nil || operator.closed != 1 {
			t.Fatalf("invalid size accepted or source leaked: %v", err)
		}
	}
	client, ref, operator := hydrationReaderFixture(t, "Subject: fixture\r\n\r\nbody")
	operator.closeErr = errors.New("source close failure")
	if got, err := client.GetRawSource(context.Background(), ref); got != "" || !errors.Is(err, operator.closeErr) || operator.closed != 1 {
		t.Fatalf("close failure lost: %q %v", got, err)
	}
}

func TestHydrationCopyPreservesBoundsErrorsAndCancellation(t *testing.T) {
	for _, size := range []int64{3, 5} {
		var output strings.Builder
		if err := copyHydratedSource(context.Background(), &output, strings.NewReader("body"), size); err == nil {
			t.Fatalf("size %d mismatch accepted", size)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyHydratedSource(ctx, io.Discard, strings.NewReader("body"), 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func BenchmarkHydratedSource(b *testing.B) {
	fixture := skipBenchFixture(b, 4<<20)
	path := filepath.Join(b.TempDir(), "message.eml")
	if err := os.WriteFile(path, fixture, 0o600); err != nil {
		b.Fatal(err)
	}
	for _, mode := range []string{"owned-string-baseline", "reader"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(fixture)))
			for range b.N {
				file, err := os.Open(path)
				if err != nil {
					b.Fatal(err)
				}
				var message mail.Message
				if mode == "reader" {
					message, err = messageFromRawReader(context.Background(), mail.Message{}, mail.MessageSummary{}, file)
				} else {
					raw := make([]byte, len(fixture))
					_, err = io.ReadFull(file, raw)
					if err == nil {
						message, err = messageFromRawFallback(context.Background(), mail.Message{}, mail.MessageSummary{}, string(raw))
					}
				}
				err = errors.Join(err, file.Close())
				if err != nil || !message.ContentComplete || message.Content != "Bench body text" || len(message.Attachments) != 1 || message.Attachments[0].Size != 4<<20 {
					b.Fatalf("hydrated fixture mismatch: %v", err)
				}
			}
		})
	}
}
