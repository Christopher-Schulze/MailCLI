package imapclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"mailcli/internal/transport"
)

const fetchMemoryThreshold = 1 << 20

// FetchMessageReader publishes a replayable source only after the complete
// FETCH response confirms the requested identity and final tagged success.
// The caller owns Close; the pooled connection has already been released.
func (c *Client) FetchMessageReader(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid uint32, expectedUIDValidity uint32, maxBytes int64) (io.ReadSeekCloser, int64, error) {
	source, err := c.fetchMessage(ctx, cfg, mailbox, uid, expectedUIDValidity, maxBytes, true)
	if err != nil {
		return nil, 0, err
	}
	return source, source.size, nil
}

type fetchSource struct {
	io.ReadSeeker
	data      []byte
	file      *os.File
	size      int64
	closeOnce sync.Once
	closeErr  error
}

func (source *fetchSource) Close() error {
	if source == nil {
		return nil
	}
	source.closeOnce.Do(func() {
		if source.file != nil {
			source.closeErr = source.file.Close()
		}
	})
	return source.closeErr
}

func fetchedBytes(data []byte) *fetchSource {
	return &fetchSource{ReadSeeker: bytes.NewReader(data), data: data, size: int64(len(data))}
}

func closeFetchSources(sources []*fetchSource) error {
	var result error
	for _, source := range sources {
		result = errors.Join(result, source.Close())
	}
	return result
}

func readFetchSourceLiteral(ctx context.Context, reader io.Reader, size int, spool bool) (*fetchSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !spool {
		data := make([]byte, size)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, err
		}
		return fetchedBytes(data), nil
	}
	file, err := os.CreateTemp("", "mailcli-fetch-*")
	if err != nil {
		return nil, err
	}
	// Unlink before writing any message content. Only the open descriptor can
	// access the source; cancellation and process exit cannot leave mail behind.
	if err := os.Remove(file.Name()); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err := copyFetchLiteral(ctx, file, reader, int64(size)); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &fetchSource{ReadSeeker: file, file: file, size: int64(size)}, nil
}

func copyFetchLiteral(ctx context.Context, writer io.Writer, reader io.Reader, size int64) error {
	buffer := make([]byte, min(size, 32*1024))
	for size > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := io.ReadFull(reader, buffer[:min(int64(len(buffer)), size)])
		if count > 0 {
			written, writeErr := writer.Write(buffer[:count])
			if writeErr != nil {
				return errors.Join(writeErr, readErr)
			}
			if written != count {
				return errors.Join(io.ErrShortWrite, readErr)
			}
			size -= int64(count)
		}
		if readErr != nil {
			return readErr
		}
	}
	return ctx.Err()
}

func validateFetchResponseValidity(line, tag string, expected uint32) error {
	code, err := flagResponseCode(line, tag)
	if err != nil {
		return &transport.TransportError{Code: transport.CodeIMAPResponseMalformed, Message: "IMAP FETCH status malformed", Err: err}
	}
	fields := strings.Fields(code)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "UIDVALIDITY") || expected == 0 {
		return nil
	}
	if len(fields) != 2 {
		return &transport.TransportError{Code: transport.CodeIMAPResponseMalformed, Message: "IMAP FETCH UIDVALIDITY malformed"}
	}
	observed, err := parsePositiveUIDValue(fields[1])
	if err != nil {
		return &transport.TransportError{Code: transport.CodeIMAPResponseMalformed, Message: "IMAP FETCH UIDVALIDITY malformed", Err: err}
	}
	return checkUIDValidity(expected, observed)
}
