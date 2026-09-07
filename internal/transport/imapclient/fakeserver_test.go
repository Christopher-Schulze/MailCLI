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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type fakeServerConfig struct {
	authOK                  bool
	authPassword            string
	sentMboxes              []string
	trashMboxes             []string
	otherMboxes             []string
	listResponse            []byte
	searchMatchID           string
	searchUID               uint32
	searchUIDs              []uint32
	uidSearchResponse       []string
	appendOK                bool
	dropAppendResponse      bool
	searchDelay             time.Duration
	searchStarted           chan struct{}
	searchStartedEvents     chan<- struct{}
	searchContinue          chan struct{}
	statusStartedEvents     chan<- struct{}
	statusContinue          chan struct{}
	deliverAfterFirstSearch bool
	moveSupported           bool
	uidExpungeSupported     bool
	initialDeletedUIDs      []uint32
	fetchPayload            []byte
	fetchResponseUID        uint32
	selectFailBox           string
	dropAfterCommands       int
	omitUIDValidity         bool
	// changedUIDValidityAfter, when non-zero, makes every SELECT after the
	// first report this UIDVALIDITY instead of 12345, simulating a mailbox
	// rebuild between resolution and mutation.
	changedUIDValidityAfter int
	changedUIDValidityValue uint32
	// hugeFetchBytes, when non-zero, makes the next FETCH announce a
	// literal of that size and then close the connection without sending
	// payload bytes, simulating an oversized message.
	hugeFetchBytes int
	hugeFetchDone  bool
	statusResponse string
	statusDelay    time.Duration
	fetchDelay     time.Duration
}

type fakeServer struct {
	listener net.Listener
	cert     tls.Certificate
	config   fakeServerConfig

	mu                   sync.Mutex
	selectCalls          int
	appendCalled         bool
	appendMbox           string
	appendFlags          []string
	appendData           []byte
	appendedMessageID    string
	appendedMessageCount int
	lastSearchMessageID  string
	searchCalls          int
	storeCalled          bool
	storeUID             uint32
	storeFlags           string
	copyCalled           bool
	copyUID              uint32
	copyDst              string
	moveCalled           bool
	moveUID              uint32
	moveDst              string
	expungeCalled        bool
	uidExpungeCalled     bool
	uidExpungeUID        uint32
	deletedUIDs          map[uint32]struct{}
	connections          int
	activeConnections    int
	maxConnections       int
	commands             []string
	searchStartedOnce    sync.Once
}

type testReporter interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}

func newFakeServer(t testReporter, cfg fakeServerConfig) *fakeServer {
	t.Helper()
	cert := generateTestCert(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tl := tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert}})
	s := &fakeServer{
		listener: tl, cert: cert, config: cfg,
		deletedUIDs: make(map[uint32]struct{}, len(cfg.initialDeletedUIDs)),
	}
	for _, uid := range cfg.initialDeletedUIDs {
		s.deletedUIDs[uid] = struct{}{}
	}
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

func (s *fakeServer) bumpSelectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selectCalls++
	return s.selectCalls
}

// claimHugeFetch reports whether this FETCH must announce the oversized
// literal; exactly one call per server wins.
func (s *fakeServer) claimHugeFetch() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.config.hugeFetchBytes > 0 && !s.config.hugeFetchDone {
		s.config.hugeFetchDone = true
		return true
	}
	return false
}

func (s *fakeServer) AppendRecord() (called bool, mbox string, flags []string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendCalled, s.appendMbox, append([]string(nil), s.appendFlags...), append([]byte(nil), s.appendData...)
}

func (s *fakeServer) ConnectionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections
}

func (s *fakeServer) MaxActiveConnections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxConnections
}

func (s *fakeServer) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

func (s *fakeServer) recordCommand(command string) {
	s.mu.Lock()
	s.commands = append(s.commands, command)
	s.mu.Unlock()
}

func (s *fakeServer) SetAuthPassword(password string) {
	s.mu.Lock()
	s.config.authPassword = password
	s.mu.Unlock()
}

func (s *fakeServer) SearchCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.searchCalls
}

func (s *fakeServer) StoreCalled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storeCalled
}

func (s *fakeServer) DeletedUIDs() []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	uids := make([]uint32, 0, len(s.deletedUIDs))
	for uid := range s.deletedUIDs {
		uids = append(uids, uid)
	}
	sort.Slice(uids, func(left, right int) bool { return uids[left] < uids[right] })
	return uids
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
	s.mu.Lock()
	s.connections++
	s.activeConnections++
	s.maxConnections = max(s.maxConnections, s.activeConnections)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.activeConnections--
		s.mu.Unlock()
		_ = conn.Close()
	}()
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)
	s.writeLine(bw, "* OK [CAPABILITY IMAP4rev1] fake ready")
	commands := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		if s.config.dropAfterCommands > 0 {
			commands++
			if commands >= s.config.dropAfterCommands {
				return
			}
		}
		tag, cmd, args, perr := splitCommand(line)
		if perr != nil {
			s.writeLine(bw, tag+" BAD parse error")
			continue
		}
		s.recordCommand(protocolCommandName(cmd, args))

		switch strings.ToUpper(cmd) {
		case "LOGIN":
			s.mu.Lock()
			authOK, authPassword := s.config.authOK, s.config.authPassword
			s.mu.Unlock()
			if authOK && (authPassword == "" || (len(args) >= 2 && args[1] == authPassword)) {
				s.writeLine(bw, tag+" OK LOGIN completed")
			} else {
				s.writeLine(bw, tag+" NO Authentication failed")
			}
		case "LIST":
			if len(s.config.listResponse) > 0 {
				if _, err := bw.Write(s.config.listResponse); err != nil {
					return
				}
				if err := bw.Flush(); err != nil {
					return
				}
				s.writeLine(bw, tag+" OK LIST completed")
				continue
			}
			for _, name := range s.config.sentMboxes {
				s.writeLine(bw, fmt.Sprintf(`* LIST (\Sent) "/" %s`, quoteIMAP(name)))
			}
			for _, name := range s.config.trashMboxes {
				s.writeLine(bw, fmt.Sprintf(`* LIST (\Trash) "/" %s`, quoteIMAP(name)))
			}
			for _, name := range s.config.otherMboxes {
				s.writeLine(bw, fmt.Sprintf(`* LIST (\HasNoChildren) "/" %s`, quoteIMAP(name)))
			}
			s.writeLine(bw, tag+" OK LIST completed")
		case "SELECT", "EXAMINE":
			if s.config.selectFailBox != "" && len(args) > 0 && args[0] == s.config.selectFailBox {
				s.writeLine(bw, tag+" NO SELECT failed")
				continue
			}
			selectN := s.bumpSelectCount()
			uidvalidity := uint32(12345)
			if s.config.changedUIDValidityAfter > 0 && selectN > s.config.changedUIDValidityAfter {
				uidvalidity = s.config.changedUIDValidityValue
			}
			s.writeLine(bw, "* FLAGS (\\Answered \\Flagged \\Deleted \\Draft \\Seen)")
			if !s.config.omitUIDValidity {
				s.writeLine(bw, fmt.Sprintf("* OK [UIDVALIDITY %d] UIDs valid", uidvalidity))
			}
			s.writeLine(bw, "* 0 EXISTS")
			s.writeLine(bw, "* 0 RECENT")
			s.writeLine(bw, tag+" OK [READ-WRITE] SELECT completed")
		case "SEARCH":
			if s.config.searchDelay > 0 {
				time.Sleep(s.config.searchDelay)
			}

			queryMessageID := ""
			s.mu.Lock()
			searchMatchID := s.config.searchMatchID
			appendedMessageID := s.appendedMessageID
			if len(args) >= 3 && strings.EqualFold(args[0], "HEADER") &&
				strings.EqualFold(args[1], "Message-ID") {
				queryMessageID = args[2]
				s.lastSearchMessageID = queryMessageID
			}
			s.searchCalls++
			searchCall := s.searchCalls
			appendedMessageCount := s.appendedMessageCount
			searchStarted := s.config.searchStarted
			searchContinue := s.config.searchContinue
			s.mu.Unlock()
			if searchStarted != nil {
				s.searchStartedOnce.Do(func() { close(searchStarted) })
			}
			if s.config.searchStartedEvents != nil {
				s.config.searchStartedEvents <- struct{}{}
			}
			if searchContinue != nil {
				<-searchContinue
			}
			matchCount := 0
			if queryMessageID != "" && queryMessageID == searchMatchID {
				matchCount++
			}
			if queryMessageID != "" && queryMessageID == appendedMessageID {
				if appendedMessageCount == 0 {
					appendedMessageCount = 1
				}
				matchCount += appendedMessageCount
			}
			if matchCount > 0 {
				values := make([]string, matchCount)
				for index := range values {
					values[index] = strconv.Itoa(index + 1)
				}
				s.writeLine(bw, "* SEARCH "+strings.Join(values, " "))
			} else {
				s.writeLine(bw, "* SEARCH")
			}
			s.writeLine(bw, tag+" OK SEARCH completed")
			if s.config.deliverAfterFirstSearch && searchCall == 1 && queryMessageID != "" && matchCount == 0 {
				s.mu.Lock()
				s.appendedMessageID = queryMessageID
				s.appendedMessageCount = 1
				s.mu.Unlock()
			}
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
			if s.config.appendOK {
				messageID := messageIDFromMessage(data)
				if messageID == "" {
					messageID = s.lastSearchMessageID
				}
				if s.appendedMessageID == messageID {
					s.appendedMessageCount++
				} else {
					s.appendedMessageID = messageID
					s.appendedMessageCount = 1
				}
			}
			s.mu.Unlock()
			if s.config.dropAppendResponse {
				return
			}
			if s.config.appendOK {
				s.writeLine(bw, tag+" OK [APPENDUID 1 100] APPEND completed")
			} else {
				s.writeLine(bw, tag+" NO APPEND failed")
			}
		case "STATUS":
			mbox := ""
			if len(args) > 0 {
				mbox = args[0]
			}
			if s.config.statusStartedEvents != nil {
				s.config.statusStartedEvents <- struct{}{}
			}
			if s.config.statusContinue != nil {
				<-s.config.statusContinue
			}
			if s.config.statusDelay > 0 {
				time.Sleep(s.config.statusDelay)
			}
			if s.config.statusResponse != "" {
				_, _ = bw.WriteString(strings.ReplaceAll(s.config.statusResponse, "<tag>", tag))
				_ = bw.Flush()
				continue
			}
			s.writeLine(bw, fmt.Sprintf(`* STATUS %s (MESSAGES 42 UNSEEN 3 UIDNEXT 100 UIDVALIDITY 12345)`, quoteIMAP(mbox)))
			s.writeLine(bw, tag+" OK STATUS completed")
		case "EXPUNGE":
			s.mu.Lock()
			expunged := len(s.deletedUIDs)
			clear(s.deletedUIDs)
			s.expungeCalled = true
			s.mu.Unlock()
			s.writeLine(bw, fmt.Sprintf("* %d EXPUNGE", expunged))
			s.writeLine(bw, tag+" OK EXPUNGE completed")
		case "UID":
			if len(args) == 0 {
				s.writeLine(bw, tag+" BAD UID missing sub-command")
				continue
			}
			subCmd := strings.ToUpper(args[0])
			switch subCmd {
			case "SEARCH":
				if len(s.config.uidSearchResponse) > 0 {
					for _, responseLine := range s.config.uidSearchResponse {
						s.writeLine(bw, strings.ReplaceAll(responseLine, "<tag>", tag))
					}
					continue
				}
				if len(args) == 2 && strings.EqualFold(args[1], "DELETED") {
					uids := s.DeletedUIDs()
					values := make([]string, len(uids))
					for index, uid := range uids {
						values[index] = strconv.FormatUint(uint64(uid), 10)
					}
					if len(values) == 0 {
						s.writeLine(bw, "* SEARCH")
					} else {
						s.writeLine(bw, "* SEARCH "+strings.Join(values, " "))
					}
					s.writeLine(bw, tag+" OK SEARCH completed")
					continue
				}
				match := false
				s.mu.Lock()
				searchMatchID := s.config.searchMatchID
				appendedMessageID := s.appendedMessageID
				if len(args) >= 4 && strings.EqualFold(args[1], "HEADER") &&
					strings.EqualFold(args[2], "Message-ID") {
					s.lastSearchMessageID = args[3]
				}
				searchStarted := s.config.searchStarted
				searchContinue := s.config.searchContinue
				s.mu.Unlock()
				if searchStarted != nil {
					s.searchStartedOnce.Do(func() { close(searchStarted) })
				}
				if s.config.searchStartedEvents != nil {
					s.config.searchStartedEvents <- struct{}{}
				}
				if searchContinue != nil {
					<-searchContinue
				}
				if (searchMatchID != "" || appendedMessageID != "") && len(args) >= 4 {
					if strings.EqualFold(args[1], "HEADER") && strings.EqualFold(args[2], "Message-ID") {
						if args[3] == searchMatchID || args[3] == appendedMessageID {
							match = true
						}
					}
				}
				if match {
					uids := append([]uint32(nil), s.config.searchUIDs...)
					if len(uids) == 0 {
						uid := s.config.searchUID
						if uid == 0 {
							uid = 42
						}
						uids = []uint32{uid}
					}
					values := make([]string, len(uids))
					for index, uid := range uids {
						values[index] = strconv.FormatUint(uint64(uid), 10)
					}
					s.writeLine(bw, "* SEARCH "+strings.Join(values, " "))
				} else {
					s.writeLine(bw, "* SEARCH")
				}
				s.writeLine(bw, tag+" OK SEARCH completed")
			case "STORE":
				uid := 0
				if len(args) > 1 {
					uid, _ = strconv.Atoi(args[1])
				}
				s.mu.Lock()
				s.storeCalled = true
				s.storeUID = uint32(uid)
				if len(args) > 2 {
					s.storeFlags = strings.Join(args[2:], " ")
				}
				if strings.Contains(strings.ToLower(strings.Join(args[2:], " ")), "\\deleted") {
					s.deletedUIDs[uint32(uid)] = struct{}{}
				}
				s.mu.Unlock()
				s.writeLine(bw, fmt.Sprintf("* 1 FETCH (UID %d FLAGS (\\Seen))", uid))
				s.writeLine(bw, tag+" OK STORE completed")
			case "EXPUNGE":
				if !s.config.uidExpungeSupported {
					s.writeLine(bw, tag+" BAD UID EXPUNGE unsupported")
					continue
				}
				uid := 0
				if len(args) > 1 {
					uid, _ = strconv.Atoi(args[1])
				}
				s.mu.Lock()
				delete(s.deletedUIDs, uint32(uid))
				s.uidExpungeCalled = true
				s.uidExpungeUID = uint32(uid)
				s.mu.Unlock()
				s.writeLine(bw, "* 1 EXPUNGE")
				s.writeLine(bw, tag+" OK UID EXPUNGE completed")
			case "COPY":
				uid := 0
				dst := ""
				if len(args) > 1 {
					uid, _ = strconv.Atoi(args[1])
				}
				if len(args) > 2 {
					dst = args[2]
				}
				s.mu.Lock()
				s.copyCalled = true
				s.copyUID = uint32(uid)
				s.copyDst = dst
				s.mu.Unlock()
				s.writeLine(bw, fmt.Sprintf("* OK [COPYUID 1 %d 100] COPY completed", uid))
				s.writeLine(bw, tag+" OK COPY completed")
			case "MOVE":
				if !s.config.moveSupported {
					s.writeLine(bw, tag+" BAD unrecognized command")
					continue
				}
				uid := 0
				dst := ""
				if len(args) > 1 {
					uid, _ = strconv.Atoi(args[1])
				}
				if len(args) > 2 {
					dst = args[2]
				}
				s.mu.Lock()
				s.moveCalled = true
				s.moveUID = uint32(uid)
				s.moveDst = dst
				s.mu.Unlock()
				s.writeLine(bw, tag+" OK MOVE completed")
			case "FETCH":
				if s.config.fetchDelay > 0 {
					time.Sleep(s.config.fetchDelay)
				}
				uid := 0
				if len(args) > 1 {
					uid, _ = strconv.Atoi(args[1])
				}
				if s.claimHugeFetch() {
					s.writeLine(bw, fmt.Sprintf("* 1 FETCH (UID %d BODY[] {%d}", uid, s.config.hugeFetchBytes))
					return
				}
				payload := s.config.fetchPayload
				if len(payload) == 0 {
					payload = []byte("From: test@example.com\r\nSubject: Test\r\n\r\nBody\r\n")
				}
				responseUID := s.config.fetchResponseUID
				if responseUID == 0 {
					responseUID = uint32(uid)
				}
				s.writeLine(bw, fmt.Sprintf("* 1 FETCH (UID %d BODY[] {%d}", responseUID, len(payload)))
				if _, err := bw.Write(payload); err != nil {
					return
				}
				s.writeLine(bw, ")")
				s.writeLine(bw, tag+" OK FETCH completed")
			default:
				s.writeLine(bw, tag+" BAD unsupported UID sub-command")
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

func protocolCommandName(command string, args []string) string {
	command = strings.ToUpper(command)
	if command == "UID" && len(args) > 0 {
		return command + " " + strings.ToUpper(args[0])
	}
	return command
}

func (s *fakeServer) writeLine(bw *bufio.Writer, line string) {
	if _, err := bw.WriteString(line + "\r\n"); err != nil {
		return
	}
	_ = bw.Flush()
}

func generateTestCert(t testReporter) tls.Certificate {
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

func messageIDFromMessage(data []byte) string {
	for _, line := range strings.Split(string(data), "\r\n") {
		if strings.HasPrefix(line, "Message-ID: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Message-ID: "))
		}
	}
	return ""
}
