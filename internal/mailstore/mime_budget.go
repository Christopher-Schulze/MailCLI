package mailstore

import (
	"context"
	"fmt"
	"io"
)

const (
	maximumHeaderBytes       = 1024 * 1024
	maximumTextPartBytes     = 16 * 1024 * 1024
	maximumMIMETextBytes     = int64(32 * 1024 * 1024)
	maximumMIMEParts         = int64(4096)
	maximumMIMEDepth         = int64(64)
	maximumMIMEMetadata      = int64(8 * 1024 * 1024)
	maximumMIMERawBytes      = maximumRFCSourceBytes
	maximumInt64             = int64(1<<63 - 1)
	mimeHeaderReaderBuffer   = 8 * 1024
	mimeHeaderInitialBytes   = 4 * 1024
	mimePartMetadataOverhead = int64(96)
)

type mimeBudgetResource string

const (
	mimeBudgetTextBytes      mimeBudgetResource = "text_bytes"
	mimeBudgetParts          mimeBudgetResource = "parts"
	mimeBudgetDepth          mimeBudgetResource = "depth"
	mimeBudgetMetadata       mimeBudgetResource = "metadata_bytes"
	mimeBudgetRawBytes       mimeBudgetResource = "raw_bytes"
	mimeBudgetDiagnosticID                      = "mime:budget:"
	mimeCanceledDiagnosticID                    = "mime:canceled"
)

type mimeParseBudgetLimits struct {
	textBytes int64
	parts     int64
	depth     int64
	metadata  int64
	rawBytes  int64
}

type mimeResourceLimitError struct {
	resource mimeBudgetResource
	used     int64
	limit    int64
}

func (e *mimeResourceLimitError) Error() string {
	return fmt.Sprintf("MIME %s budget exceeded (%d/%d bytes or items)", e.resource, e.used, e.limit)
}

func (e *mimeResourceLimitError) ErrorCode() string {
	return "mime_resource_limit"
}

type mimeParseBudget struct {
	limits    mimeParseBudgetLimits
	textBytes int64
	parts     int64
	metadata  int64
	rawBytes  int64
	exhausted *mimeResourceLimitError
}

func defaultMIMEParseBudgetLimits() mimeParseBudgetLimits {
	return mimeParseBudgetLimits{
		textBytes: maximumMIMETextBytes,
		parts:     maximumMIMEParts,
		depth:     maximumMIMEDepth,
		metadata:  maximumMIMEMetadata,
		rawBytes:  maximumMIMERawBytes,
	}
}

func newMIMEParseBudget(limits mimeParseBudgetLimits) *mimeParseBudget {
	if limits.textBytes < 0 {
		limits.textBytes = 0
	}
	if limits.parts < 0 {
		limits.parts = 0
	}
	if limits.depth < 0 {
		limits.depth = 0
	}
	if limits.metadata < 0 {
		limits.metadata = 0
	}
	if limits.rawBytes < 0 {
		limits.rawBytes = 0
	}
	return &mimeParseBudget{limits: limits}
}

func (b *mimeParseBudget) error() *mimeResourceLimitError {
	return b.exhausted
}

func (b *mimeParseBudget) exhaust(resource mimeBudgetResource, used, limit int64) *mimeResourceLimitError {
	if b.exhausted != nil {
		return b.exhausted
	}
	if used < 0 {
		used = maximumInt64
	}
	if limit < 0 {
		limit = 0
	}
	b.exhausted = &mimeResourceLimitError{resource: resource, used: used, limit: limit}
	return b.exhausted
}

func (b *mimeParseBudget) reserve(resource mimeBudgetResource, used *int64, amount, limit int64) bool {
	if b.exhausted != nil {
		return false
	}
	if amount < 0 || *used > limit || amount > limit-*used {
		candidate := *used
		if amount > 0 && candidate <= maximumInt64-amount {
			candidate += amount
		} else {
			candidate = maximumInt64
		}
		return b.exhaust(resource, candidate, limit) == nil
	}
	*used += amount
	return true
}

func (b *mimeParseBudget) visit(depth int64) *mimeResourceLimitError {
	if err := b.checkDepth(depth); err != nil {
		return err
	}
	if !b.reserve(mimeBudgetParts, &b.parts, 1, b.limits.parts) {
		return b.error()
	}
	return nil
}

func (b *mimeParseBudget) checkDepth(depth int64) *mimeResourceLimitError {
	if depth < 0 || depth >= b.limits.depth {
		return b.exhaust(mimeBudgetDepth, depth+1, b.limits.depth)
	}
	return nil
}

func (b *mimeParseBudget) remainingTextBytes() int64 {
	if b.exhausted != nil || b.textBytes >= b.limits.textBytes {
		return 0
	}
	return b.limits.textBytes - b.textBytes
}

func (b *mimeParseBudget) consumeText(amount int64) bool {
	return b.reserve(mimeBudgetTextBytes, &b.textBytes, amount, b.limits.textBytes)
}

func (b *mimeParseBudget) reserveMetadata(amount int64) bool {
	return b.reserve(mimeBudgetMetadata, &b.metadata, amount, b.limits.metadata)
}

type mimeBudgetReader struct {
	ctx    context.Context
	reader io.Reader
	budget *mimeParseBudget
}

func (r *mimeBudgetReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if err := r.budget.error(); err != nil {
		return 0, err
	}
	remaining := r.budget.limits.rawBytes - r.budget.rawBytes
	if remaining <= 0 {
		return 0, r.budget.exhaust(mimeBudgetRawBytes, r.budget.rawBytes+1, r.budget.limits.rawBytes)
	}
	if int64(len(buffer)) > remaining {
		buffer = buffer[:remaining]
	}
	read, err := r.reader.Read(buffer)
	if read < 0 || read > len(buffer) {
		return 0, fmt.Errorf("MIME source reader returned invalid byte count %d", read)
	}
	if read > 0 && !r.budget.reserve(mimeBudgetRawBytes, &r.budget.rawBytes, int64(read), r.budget.limits.rawBytes) {
		return read, r.budget.error()
	}
	if contextErr := r.ctx.Err(); contextErr != nil {
		return read, contextErr
	}
	return read, err
}

type mimeContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r mimeContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	read, err := r.reader.Read(buffer)
	if contextErr := r.ctx.Err(); contextErr != nil {
		return read, contextErr
	}
	return read, err
}

func closeMIMEReaderOnCancel(ctx context.Context, reader io.Reader) func() {
	if ctx == nil || ctx.Done() == nil {
		return func() {}
	}
	closer, ok := reader.(io.Closer)
	if !ok {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = closer.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}
