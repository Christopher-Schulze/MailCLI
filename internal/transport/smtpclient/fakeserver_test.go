package smtpclient

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTPServer is an in-process SMTP server implementing the subset of
// ESMTP the client uses: greeting, EHLO (STARTTLS + AUTH PLAIN), STARTTLS
// upgrade, AUTH PLAIN, MAIL, RCPT, DATA. It stores the envelope and payload
// for assertions.
type fakeSMTPServer struct {
	t  *testing.T
	ln net.Listener

	tlsCert tls.Certificate
	closed  chan struct{}

	mu        sync.Mutex
	mailFrom  string
	rcpts     []string
	data      []byte
	authCalls int

	authUser      string
	authPass      string
	authFail      bool
	rejectRcpt    string
	stallGreeting bool
	noStartTLS    bool
	// stallFinalReply withholds the 250 reply after DATA until the server
	// closes, simulating a hung server for ctx-cancel tests.
	stallFinalReply bool
}

// newFakeSMTPServer starts the fake; configure runs before the accept loop
// spawns so tests can set behavior without racing the handler goroutine.
func newFakeSMTPServer(t *testing.T, configure ...func(*fakeSMTPServer)) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeSMTPServer{
		t:        t,
		ln:       ln,
		tlsCert:  selfSignedCert(t),
		closed:   make(chan struct{}),
		authUser: "user",
		authPass: "s3cret-app-pw",
	}
	for _, c := range configure {
		c(s)
	}
	t.Cleanup(func() {
		close(s.closed)
		_ = ln.Close()
	})
	go s.serve()
	return s
}

func (s *fakeSMTPServer) host() string { return "127.0.0.1" }
func (s *fakeSMTPServer) port() int {
	_, portStr, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		s.t.Fatalf("split addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		s.t.Fatalf("port: %v", err)
	}
	return port
}

func (s *fakeSMTPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn, false)
	}
}

func (s *fakeSMTPServer) handle(conn net.Conn, isTLS bool) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)

	if s.stallGreeting {
		select {
		case <-s.closed:
		case <-time.After(30 * time.Second):
		}
		return
	}
	if !writeLine(conn, "220 fake ESMTP ready") {
		return
	}

	for {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			if s.noStartTLS {
				if !writeLine(conn, "250-fake greets you") || !writeLine(conn, "250 AUTH PLAIN") {
					return
				}
			} else {
				if !writeLine(conn, "250-fake greets you") || !writeLine(conn, "250-STARTTLS") || !writeLine(conn, "250 AUTH PLAIN") {
					return
				}
			}
		case upper == "STARTTLS":
			if isTLS {
				if !writeLine(conn, "503 5.5.1 Already in TLS") {
					return
				}
				continue
			}
			if !writeLine(conn, "220 2.0.0 Ready to start TLS") {
				return
			}
			tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{s.tlsCert}})
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			r = bufio.NewReader(tlsConn)
			isTLS = true
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			s.mu.Lock()
			s.authCalls++
			s.mu.Unlock()
			payload := strings.TrimSpace(cmd[len("AUTH PLAIN"):])
			decoded, decErr := base64.StdEncoding.DecodeString(payload)
			parts := strings.SplitN(string(decoded), "\x00", 3)
			if decErr != nil || len(parts) != 3 || parts[1] != s.authUser || parts[2] != s.authPass || s.authFail {
				if !writeLine(conn, "535 5.7.8 Error: authentication failed") {
					return
				}
				continue
			}
			if !writeLine(conn, "235 2.7.0 Accepted") {
				return
			}
		case strings.HasPrefix(upper, "MAIL FROM:"):
			s.mu.Lock()
			s.mailFrom = extractAddress(cmd)
			s.mu.Unlock()
			if !writeLine(conn, "250 2.1.0 OK") {
				return
			}
		case strings.HasPrefix(upper, "RCPT TO:"):
			addr := extractAddress(cmd)
			if addr == s.rejectRcpt {
				if !writeLine(conn, "550 5.1.1 User unknown") {
					return
				}
				continue
			}
			s.mu.Lock()
			s.rcpts = append(s.rcpts, addr)
			s.mu.Unlock()
			if !writeLine(conn, "250 2.1.5 OK") {
				return
			}
		case upper == "DATA":
			if !writeLine(conn, "354 End data with <CR><LF>.<CR><LF>") {
				return
			}
			payload, err := readData(r)
			if err != nil {
				return
			}
			s.mu.Lock()
			s.data = payload
			s.mu.Unlock()
			if s.stallFinalReply {
				select {
				case <-s.closed:
					return
				case <-time.After(30 * time.Second):
					return
				}
			}
			if !writeLine(conn, "250 2.0.0 OK: queued") {
				return
			}
		case upper == "QUIT":
			writeLine(conn, "221 2.0.0 Bye")
			return
		default:
			if !writeLine(conn, "500 5.5.2 Unrecognized command") {
				return
			}
		}
	}
}

func writeLine(conn net.Conn, line string) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := conn.Write([]byte(line + "\r\n"))
	return err == nil
}

// readData reads the DATA payload until the terminating "." line, undoing
// dot-stuffing.
func readData(r *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "." {
			return buf.Bytes(), nil
		}
		trimmed = strings.TrimPrefix(trimmed, ".")
		buf.WriteString(trimmed)
		buf.WriteString("\r\n")
	}
}

// extractAddress pulls the addr-spec out of "MAIL FROM:<a@b>" style commands.
func extractAddress(cmd string) string {
	i := strings.Index(cmd, ":")
	if i < 0 {
		return ""
	}
	addr := strings.TrimSpace(cmd[i+1:])
	return strings.TrimSuffix(strings.TrimPrefix(addr, "<"), ">")
}

// selfSignedCert generates a throwaway certificate for the fake server.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
