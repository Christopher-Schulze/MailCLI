package imapclient

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeServerConfig struct {
	authOK        bool
	sentMboxes    []string
	otherMboxes   []string
	searchMatchID string
	appendOK      bool
	searchDelay   time.Duration
}

type fakeServer struct {
	t        *testing.T
	listener net.Listener
	cert     tls.Certificate
	config   fakeServerConfig

	mu           sync.Mutex
	appendCalled bool
	appendMbox   string
	appendFlags  []string
	appendData   []byte
}

func newFakeServer(t *testing.T, cfg fakeServerConfig) *fakeServer {
	t.Helper()
	cert := generateTestCert(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tl := tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert}})
	s := &fakeServer{t: t, listener: tl, cert: cert, config: cfg}
	go s.run()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func (s *fakeServer) Close() error {
	return s.listener.Close()
}

func (s *fakeServer) Addr() string {
	return s.listener.Addr().String()
}

func (s *fakeServer) AppendRecord() (called bool, mbox string, flags []string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendCalled, s.appendMbox, append([]string(nil), s.appendFlags...), append([]byte(nil), s.appendData...)
}

func (s *fakeServer) run() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)
	s.writeLine(bw, "* OK [CAPABILITY IMAP4rev1] fake ready")

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}

		tag, cmd, args, perr := splitCommand(line)
		if perr != nil {
			s.writeLine(bw, tag+" BAD parse error")
			continue
		}

		switch strings.ToUpper(cmd) {
		case "LOGIN":
			if s.config.authOK {
				s.writeLine(bw, tag+" OK LOGIN completed")
			} else {
				s.writeLine(bw, tag+" NO Authentication failed")
			}
		case "LIST":
			for _, name := range s.config.sentMboxes {
				s.writeLine(bw, fmt.Sprintf(`* LIST (\Sent) "/" %s`, quoteIMAP(name)))
			}
			for _, name := range s.config.otherMboxes {
				s.writeLine(bw, fmt.Sprintf(`* LIST (\HasNoChildren) "/" %s`, quoteIMAP(name)))
			}
			s.writeLine(bw, tag+" OK LIST completed")
		case "SELECT", "EXAMINE":
			s.writeLine(bw, "* FLAGS (\\Answered \\Flagged \\Deleted \\Draft \\Seen)")
			s.writeLine(bw, "* 0 EXISTS")
			s.writeLine(bw, "* 0 RECENT")
			s.writeLine(bw, tag+" OK [READ-WRITE] SELECT completed")
		case "SEARCH":
			if s.config.searchDelay > 0 {
				time.Sleep(s.config.searchDelay)
			}

			match := false
			if s.config.searchMatchID != "" && len(args) >= 3 {
				if strings.EqualFold(args[0], "HEADER") && strings.EqualFold(args[1], "Message-ID") {
					if args[2] == s.config.searchMatchID {
						match = true
					}
				}
			}
			if match {
				s.writeLine(bw, "* SEARCH 1")
			} else {
				s.writeLine(bw, "* SEARCH")
			}
			s.writeLine(bw, tag+" OK SEARCH completed")
		case "APPEND":
			if len(args) < 3 {
				s.writeLine(bw, tag+" BAD APPEND syntax")
				continue
			}
			mbox := args[0]
			flags := parseFlagList(args[1])
			lit := args[2]
			if !strings.HasPrefix(lit, "{") || !strings.HasSuffix(lit, "}") {
				s.writeLine(bw, tag+" BAD literal")
				continue
			}
			n, err := strconv.Atoi(lit[1 : len(lit)-1])
			if err != nil {
				s.writeLine(bw, tag+" BAD literal length")
				continue
			}
			s.writeLine(bw, "+ go ahead")
			data := make([]byte, n)
			if _, err := io.ReadFull(br, data); err != nil {
				return
			}
			crlf := make([]byte, 2)
			if _, err := io.ReadFull(br, crlf); err != nil || crlf[0] != '\r' || crlf[1] != '\n' {
				s.writeLine(bw, tag+" BAD expected CRLF after literal")
				return
			}
			s.mu.Lock()
			s.appendCalled = true
			s.appendMbox = mbox
			s.appendFlags = flags
			s.appendData = data
			s.mu.Unlock()
			if s.config.appendOK {
				s.writeLine(bw, tag+" OK [APPENDUID 1 100] APPEND completed")
			} else {
				s.writeLine(bw, tag+" NO APPEND failed")
			}
		case "LOGOUT":
			s.writeLine(bw, "* BYE")
			s.writeLine(bw, tag+" OK LOGOUT completed")
			return
		default:
			s.writeLine(bw, tag+" BAD unknown command")
		}
	}
}

func (s *fakeServer) writeLine(bw *bufio.Writer, line string) {
	if _, err := bw.WriteString(line + "\r\n"); err != nil {
		return
	}
	_ = bw.Flush()
}

func generateTestCert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"MailCLI Test"}},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("x509 key pair: %v", err)
	}
	return cert
}

func splitCommand(line string) (tag, cmd string, args []string, err error) {
	s := strings.TrimLeft(line, " ")
	i := 0
	for i < len(s) && s[i] != ' ' {
		i++
	}
	if i == len(s) {
		return "", "", nil, fmt.Errorf("no tag")
	}
	tag = s[:i]
	s = strings.TrimLeft(s[i:], " ")
	i = 0
	for i < len(s) && s[i] != ' ' {
		i++
	}
	cmd = s[:i]
	s = s[i:]

	for {
		s = strings.TrimLeft(s, " ")
		if s == "" {
			break
		}
		arg, rest, perr := parseIMAPArg(s)
		if perr != nil {
			return "", "", nil, perr
		}
		args = append(args, arg)
		s = rest
	}
	return tag, cmd, args, nil
}

func parseIMAPArg(s string) (arg, rest string, err error) {
	if s == "" {
		return "", "", fmt.Errorf("empty arg")
	}
	s = strings.TrimLeft(s, " ")
	if s == "" {
		return "", "", fmt.Errorf("empty arg")
	}
	switch s[0] {
	case '"':
		return parseQuoted(s)
	case '(':
		depth := 0
		for i := 0; i < len(s); i++ {
			if s[i] == '\\' {
				i++
				continue
			}
			if s[i] == '(' {
				depth++
			}
			if s[i] == ')' {
				depth--
				if depth == 0 {
					return s[:i+1], s[i+1:], nil
				}
			}
		}
		return "", s, fmt.Errorf("unterminated parenthesized")
	case '{':
		i := strings.Index(s, "}")
		if i < 0 {
			return "", s, fmt.Errorf("unterminated literal")
		}
		return s[:i+1], s[i+1:], nil
	default:
		i := 0
		for i < len(s) && s[i] != ' ' {
			i++
		}
		return s[:i], s[i:], nil
	}
}

func parseFlagList(s string) []string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		s = s[1 : len(s)-1]
	}
	var flags []string
	for _, f := range strings.Fields(s) {
		if f != "" {
			flags = append(flags, f)
		}
	}
	return flags
}
