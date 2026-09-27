package imapclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"mailcli/internal/transport"
	"net"
	"strconv"
	"testing"
)

func TestFetchMessagePrefixWireBounds(t *testing.T) {
	for _, test := range []struct {
		name, section string
		bound         int64
		wantCode      string
	}{
		{"bounded prefix", "BODY[]<0>", 4, ""},
		{"over cap", "BODY[]<0>", 3, transport.CodeIMAPRawSourceTooLarge},
		{"wrong offset", "BODY[]<1>", 4, transport.CodeIMAPResponseMalformed},
		{"wrong section", "BODY[HEADER]<0>", 4, transport.CodeIMAPResponseMalformed},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}, fetchResponse: []byte(fmt.Sprintf("* 1 FETCH (UID 42 %s {4}\r\nBody)\r\n<tag> OK FETCH completed\r\n", test.section))})
			host, rawPort, err := net.SplitHostPort(server.Addr())
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(rawPort)
			if err != nil {
				t.Fatal(err)
			}
			client := New()
			client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
			data, err := client.FetchMessagePrefix(context.Background(), transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}, "INBOX", 42, 12345, test.bound)
			if transport.ErrorCode(err) != test.wantCode || test.wantCode == "" && !bytes.Equal(data, []byte("Body")) {
				t.Fatalf("prefix=%q err=%v", data, err)
			}
		})
	}
}
