package smtpclient

import (
	"net"
	"net/textproto"

	"mailcli/internal/transport"
)

const (
	// Count received wire bytes, including TLS framing, before net/smtp can
	// accumulate an unbounded physical line or multiline response.
	maxCommandReplyBytes = 128 << 10
	// StartTLS includes the plaintext reply, verified TLS handshake and its
	// automatic post-TLS EHLO. Go's individual TLS message limits still apply.
	maxStartTLSReplyBytes = 1 << 20
)

type replyBudgetConn struct {
	net.Conn
	remaining int
}

func (c *replyBudgetConn) Read(p []byte) (int, error) {
	if c.remaining == 0 {
		return 0, &transport.TransportError{
			Code: transport.CodeSMTPRejected, Message: "SMTP inbound reply budget exceeded",
			Err: textproto.ProtocolError("SMTP inbound reply budget exceeded"),
		}
	}
	n, err := c.Conn.Read(p[:min(len(p), c.remaining)])
	c.remaining -= n
	return n, err
}
