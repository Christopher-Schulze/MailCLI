package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"golang.org/x/net/html"
)

const (
	maximumIncomingHTMLBytes  = 16 << 20
	maximumIncomingHTMLTokens = 262144
	maximumIncomingHTMLNodes  = 262144
	maximumIncomingTextBytes  = 32 << 20
)

// HTMLConversionError identifies the failed conversion phase. No partial
// rendered string is returned; MIME callers can retain other valid parts.
type HTMLConversionError struct {
	Stage string
	Cause error
}

func (err *HTMLConversionError) Error() string {
	return fmt.Sprintf("incoming HTML %s: %v", err.Stage, err.Cause)
}

func (err *HTMLConversionError) Unwrap() error { return err.Cause }

// HTMLToPlainTextContext preserves the incoming rendering policy while bounding
// source, lexical work, the repaired tree and output. maximumBytes is a positive
// remaining output allowance, capped at 32 MiB. Source and output never alias.
func HTMLToPlainTextContext(ctx context.Context, source []byte, maximumBytes int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(source) > maximumIncomingHTMLBytes {
		return "", &HTMLConversionError{Stage: "source", Cause: fmt.Errorf("source exceeds 16 MiB")}
	}
	if maximumBytes <= 0 || maximumBytes > maximumIncomingTextBytes {
		return "", &HTMLConversionError{Stage: "render", Cause: fmt.Errorf("output allowance must be between 1 and 32 MiB")}
	}
	if err := checkIncomingHTMLTokens(ctx, source, maximumIncomingHTMLTokens); err != nil {
		return "", err
	}
	root, err := html.Parse(contextReader{ctx: ctx, reader: bytes.NewReader(source)})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return "", contextErr
		}
		return "", &HTMLConversionError{Stage: "parse", Cause: err}
	}
	if err := checkIncomingHTMLTree(ctx, root, maximumIncomingHTMLNodes); err != nil {
		return "", err
	}
	text, err := renderDraftText(ctx, root, maximumBytes)
	if err != nil && ctx.Err() == nil {
		return "", &HTMLConversionError{Stage: "render", Cause: err}
	}
	return text, err
}

func checkIncomingHTMLTokens(ctx context.Context, source []byte, maximum int) error {
	// Each non-error token consumes at least one source byte. Small inputs
	// cannot exceed this lexical budget and need no second parsing pass.
	// Non-text tokens consume a '<'; at most one text token separates them.
	// Sparse markup therefore also needs no tokenizer buffer or second pass.
	if len(source) <= maximum || bytes.Count(source, []byte("<")) <= (maximum-1)/2 {
		return ctx.Err()
	}
	tokenizer := html.NewTokenizer(contextReader{ctx: ctx, reader: bytes.NewReader(source)})
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if tokenizer.Next() == html.ErrorToken {
			if errors.Is(tokenizer.Err(), io.EOF) {
				return nil
			}
			return tokenizer.Err()
		}
		if count >= maximum {
			return &HTMLConversionError{Stage: "tokens", Cause: fmt.Errorf("more than %d lexical tokens", maximum)}
		}
	}
}

func checkIncomingHTMLTree(ctx context.Context, root *html.Node, maximum int) error {
	count, depth := 0, 0
	for node := root; node != nil; {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > maximum || depth > 512 {
			return &HTMLConversionError{Stage: "tree", Cause: fmt.Errorf("more than %d nodes or 512 levels", maximum)}
		}
		if node.FirstChild != nil {
			node = node.FirstChild
			depth++
			continue
		}
		for node != root && node.NextSibling == nil {
			node = node.Parent
			depth--
		}
		if node == root {
			break
		}
		node = node.NextSibling
	}
	return nil
}
