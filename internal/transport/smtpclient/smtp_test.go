package smtpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/smtp"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mailcli/internal/transport"
)

const testMessage = "From: a@b.c\r\nTo: d@e.f\r\nMessage-ID: <m1@a.b>\r\nSubject: t\r\n\r\nline1\r\n.hidden\r\n\r\n"

func testClient() *Client {
	return New(WithTLSConfig(&tls.Config{InsecureSkipVerify: true})) // test-only: fake's self-signed cert
}

func TestSubmitSuccess(t *testing.T) {
	srv := newFakeSMTPServer(t)
	cfg := transport.SubmitConfig{Host: srv.host(), Port: srv.port(), Username: "user", Password: "s3cret-app-pw"}

	ev, err := testClient().Submit(context.Background(), cfg, "a@b.c",
		[]string{"d@e.f", "d@e.f", "g@h.i"}, []byte(testMessage))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !strings.HasPrefix(ev.ServerResponse, "250") {
		t.Errorf("ServerResponse = %q, want 250 prefix", ev.ServerResponse)
	}
	if ev.MessageID != "<m1@a.b>" {
		t.Errorf("MessageID = %q, want <m1@a.b>", ev.MessageID)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.mailFrom != "a@b.c" {
		t.Errorf("mailFrom = %q, want a@b.c", srv.mailFrom)
	}
	if want := []string{"d@e.f", "g@h.i"}; !reflect.DeepEqual(srv.rcpts, want) {
		t.Errorf("rcpts = %v, want %v (deduplicated, order-stable)", srv.rcpts, want)
	}
	if !bytes.Equal(srv.data, []byte(testMessage)) {
		t.Errorf("payload = %q, want %q", srv.data, testMessage)
	}
	if srv.authCalls != 1 {
		t.Errorf("authCalls = %d, want 1", srv.authCalls)
	}
}

func TestSubmitReaderSuccess(t *testing.T) {
	srv := newFakeSMTPServer(t)
	cfg := transport.SubmitConfig{Host: srv.host(), Port: srv.port(), Username: "user", Password: "s3cret-app-pw"}
	reader := strings.NewReader(testMessage)
	ev, err := testClient().SubmitReader(context.Background(), cfg, "a@b.c", []string{"d@e.f"}, "<streamed@a.b>", reader, int64(len(testMessage)))
	if err != nil {
		t.Fatalf("SubmitReader: %v", err)
	}
	if ev.MessageID != "<streamed@a.b>" || !strings.HasPrefix(ev.ServerResponse, "250") {
		t.Fatalf("evidence = %+v", ev)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if string(srv.data) != testMessage {
		t.Fatalf("payload = %q, want %q", srv.data, testMessage)
	}
}

func TestSubmitReaderRejectsNegativeSize(t *testing.T) {
	_, err := testClient().SubmitReader(context.Background(), transport.SubmitConfig{}, "a@b.c", []string{"d@e.f"}, "", strings.NewReader(testMessage), -1)
	if transport.ErrorCode(err) != transport.CodeSMTPRejected {
		t.Fatalf("error = %v, want smtp_rejected", err)
	}
}

func TestSubmitErrors(t *testing.T) {
	cfg := func(srv *fakeSMTPServer) transport.SubmitConfig {
		return transport.SubmitConfig{Host: srv.host(), Port: srv.port(), Username: "user", Password: "s3cret-app-pw"}
	}

	cases := []struct {
		name        string
		setup       func(*fakeSMTPServer)
		wantCode    string
		wantContain string
	}{
		{
			name:     "auth failure",
			setup:    func(s *fakeSMTPServer) { s.authFail = true },
			wantCode: transport.CodeSMTPAuthFailed,
		},
		{
			name:        "rcpt rejection",
			setup:       func(s *fakeSMTPServer) { s.rejectRcpt = "bad@x.y" },
			wantCode:    transport.CodeSMTPRejected,
			wantContain: "User unknown",
		},
		{
			name:     "no STARTTLS advertised",
			setup:    func(s *fakeSMTPServer) { s.noStartTLS = true },
			wantCode: transport.CodeSMTPTLSFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeSMTPServer(t, tc.setup)

			_, err := testClient().Submit(context.Background(), cfg(srv), "a@b.c",
				[]string{"d@e.f", "bad@x.y"}, []byte(testMessage))
			if err == nil {
				t.Fatal("Submit: want error, got nil")
			}
			if got := transport.ErrorCode(err); got != tc.wantCode {
				t.Errorf("code = %q, want %q (err: %v)", got, tc.wantCode, err)
			}
			if tc.wantContain != "" && !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err, tc.wantContain)
			}
			if strings.Contains(err.Error(), "s3cret-app-pw") {
				t.Errorf("error %q leaks the password", err)
			}
		})
	}
}

func TestSubmitDefaultTLSRejectsSelfSigned(t *testing.T) {
	srv := newFakeSMTPServer(t)
	cfg := transport.SubmitConfig{Host: srv.host(), Port: srv.port(), Username: "user", Password: "s3cret-app-pw"}

	// Production path: no injected TLS config, default verification must fail
	// against the fake's self-signed certificate.
	_, err := New().Submit(context.Background(), cfg, "a@b.c", []string{"d@e.f"}, []byte(testMessage))
	if err == nil {
		t.Fatal("Submit: want TLS failure, got nil")
	}
	if got := transport.ErrorCode(err); got != transport.CodeSMTPTLSFailed {
		t.Errorf("code = %q, want %q (err: %v)", got, transport.CodeSMTPTLSFailed, err)
	}
}

func TestSubmitContextCanceledBeforeStart(t *testing.T) {
	srv := newFakeSMTPServer(t)
	cfg := transport.SubmitConfig{Host: srv.host(), Port: srv.port(), Username: "user", Password: "s3cret-app-pw"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := testClient().Submit(ctx, cfg, "a@b.c", []string{"d@e.f"}, []byte(testMessage))
	if err == nil {
		t.Fatal("Submit: want error, got nil")
	}
	if got := transport.ErrorCode(err); got != transport.CodeSMTPTimeout {
		t.Errorf("code = %q, want %q (err: %v)", got, transport.CodeSMTPTimeout, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Submit took %v after pre-canceled ctx", elapsed)
	}
}

func TestSubmitContextDeadlineDuringGreeting(t *testing.T) {
	srv := newFakeSMTPServer(t, func(s *fakeSMTPServer) { s.stallGreeting = true })
	cfg := transport.SubmitConfig{Host: srv.host(), Port: srv.port(), Username: "user", Password: "s3cret-app-pw"}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := testClient().Submit(ctx, cfg, "a@b.c", []string{"d@e.f"}, []byte(testMessage))
	if err == nil {
		t.Fatal("Submit: want error, got nil")
	}
	if got := transport.ErrorCode(err); got != transport.CodeSMTPTimeout {
		t.Errorf("code = %q, want %q (err: %v)", got, transport.CodeSMTPTimeout, err)
	}
	// Must abort well under the 10s dial / 30s command budgets.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Submit took %v, want abort within ~150ms", elapsed)
	}
}

func TestSubmitNoDoubleCloseOnCancel(t *testing.T) {
	srv := newFakeSMTPServer(t)
	cfg := transport.SubmitConfig{Host: srv.host(), Port: srv.port(), Username: "user", Password: "s3cret-app-pw"}
	// Cancel before the call; the sync.Once close guard must prevent
	// a double-close panic when both the context goroutine and the
	// deferred cleanup run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = testClient().Submit(ctx, cfg, "a@b.c", []string{"d@e.f"}, []byte(testMessage))
	// A second call on a fresh cancelled context must also work without panic.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_, _ = testClient().Submit(ctx2, cfg, "a@b.c", []string{"d@e.f"}, []byte(testMessage))
}

func TestTransferBudgetForSize(t *testing.T) {
	if got := transferBudgetForSize(0); got != commandBudget {
		t.Fatalf("empty payload budget = %v, want command budget %v", got, commandBudget)
	}
	if got := transferBudgetForSize(512 << 20); got != commandBudget+512*time.Second {
		t.Fatalf("512 MiB budget = %v, want %v", got, commandBudget+512*time.Second)
	}
	if got := transferBudgetForSize(100 << 30); got != transferCap {
		t.Fatalf("100 GiB budget = %v, want capped %v", got, transferCap)
	}
}

func bigTestMessageLines(n int) []byte {
	var b strings.Builder
	b.WriteString("From: a@b.c\r\nTo: d@e.f\r\nMessage-ID: <big@a.b>\r\nSubject: big\r\n\r\n")
	line := "payload " + strings.Repeat("x", 990) + "\r\n"
	for i := 0; i < n; i++ {
		b.WriteString(line)
	}
	return []byte(b.String())
}

// deadlineRecorder observes every SetDeadline on the SMTP connection.
type deadlineRecorder struct {
	net.Conn
	mu        sync.Mutex
	deadlines []time.Time
}

func (d *deadlineRecorder) SetDeadline(t time.Time) error {
	d.mu.Lock()
	d.deadlines = append(d.deadlines, t)
	d.mu.Unlock()
	return d.Conn.SetDeadline(t)
}

// A 10 MB payload earns a ~40 s transfer budget against the flat 30 s
// command budget. The recorder proves the payload phase ran under the
// transfer deadline and the final reply under a restored command budget:
// deterministic, no wall-clock dependence. (Loopback kernel buffers make
// timing-based proofs vacuous: macOS autotune absorbs megabytes, so a
// throttled server only delays the final reply, never the client write.)
func TestSendDataSetsTransferDeadline(t *testing.T) {
	srv := newFakeSMTPServer(t)
	raw, err := net.Dial("tcp", net.JoinHostPort(srv.host(), strconv.Itoa(srv.port())))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	rec := &deadlineRecorder{Conn: raw}
	client, err := smtp.NewClient(rec, srv.host())
	if err != nil {
		t.Fatalf("smtp client: %v", err)
	}
	ctx := context.Background()
	tlsCfg := &tls.Config{InsecureSkipVerify: true}
	if err := bumpDeadline(rec, ctx); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if err := client.Hello("localhost"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if err := client.StartTLS(tlsCfg); err != nil {
		t.Fatalf("STARTTLS: %v", err)
	}
	if err := client.Auth(smtp.PlainAuth("", "user", "s3cret-app-pw", srv.host())); err != nil {
		t.Fatalf("AUTH: %v", err)
	}
	if err := client.Mail("a@b.c"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := client.Rcpt("d@e.f"); err != nil {
		t.Fatalf("RCPT: %v", err)
	}
	msg := bigTestMessageLines(10 << 10)
	wantTransfer := transferBudgetForSize(int64(len(msg)))
	if wantTransfer <= commandBudget+5*time.Second {
		t.Fatalf("test setup: transfer budget %v not clearly above command budget %v", wantTransfer, commandBudget)
	}
	start := time.Now()
	resp, err := sendData(rec, ctx, client, bytes.NewReader(msg), int64(len(msg)))
	if err != nil {
		t.Fatalf("sendData: %v", err)
	}
	rec.mu.Lock()
	got := append([]time.Time(nil), rec.deadlines...)
	rec.mu.Unlock()
	if !strings.HasPrefix(resp, "250") {
		t.Fatalf("response = %q, want 250", resp)
	}
	transferIdx := -1
	for i, dl := range got {
		if dl.Sub(start) >= 35*time.Second && transferIdx < 0 {
			transferIdx = i
		}
	}
	if transferIdx < 0 {
		t.Fatalf("no transfer-size deadline in %v", got)
	}
	last := got[len(got)-1].Sub(start)
	if last < commandBudget-5*time.Second || last > commandBudget+5*time.Second {
		t.Fatalf("final deadline = %v after start, want restored command budget %v", last, commandBudget)
	}
	if transferIdx == len(got)-1 {
		t.Fatalf("transfer deadline is last; final reply has no restored command budget in %v", got)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !bytes.Equal(srv.data, msg) {
		t.Fatalf("payload bytes = %d, want %d intact", len(srv.data), len(msg))
	}
}

// transferError maps a live-context socket timeout to the dedicated
// transfer code, keeps done-context as command timeout, and passes
// anything else to the generic session classification.
func TestTransferErrorClassifiesTimeout(t *testing.T) {
	ctx := context.Background()
	timeoutOp := &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}
	var typed *transport.TransportError
	if err := transferError(ctx, timeoutOp); !errors.As(err, &typed) || typed.Code != transport.CodeSMTPTransferTimeout {
		t.Fatalf("transferError = %v, want smtp_transfer_timeout", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := transferError(canceled, timeoutOp); !errors.As(err, &typed) || typed.Code != transport.CodeSMTPTimeout {
		t.Fatalf("transferError = %v, want smtp_timeout on done context", err)
	}
	if err := transferError(ctx, errors.New("boom")); errors.As(err, &typed) && typed.Code == transport.CodeSMTPTransferTimeout {
		t.Fatalf("transferError = %v, plain errors must not map to transfer timeout", err)
	}
}

// A server that stalls the final reply must be terminated by context
// cancellation with a typed timeout, not by the command budget.
func TestSubmitContextCancelDuringStalledReply(t *testing.T) {
	srv := newFakeSMTPServer(t, func(s *fakeSMTPServer) { s.stallFinalReply = true })
	cfg := transport.SubmitConfig{Host: srv.host(), Port: srv.port(), Username: "user", Password: "s3cret-app-pw"}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := testClient().Submit(ctx, cfg, "a@b.c", []string{"d@e.f"}, []byte(testMessage))
	elapsed := time.Since(start)
	var typed *transport.TransportError
	if !errors.As(err, &typed) || typed.Code != transport.CodeSMTPTimeout {
		t.Fatalf("Submit error = %v, want smtp_timeout", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Submit took %v, want cancel-driven abort well under the command budget", elapsed)
	}
}
