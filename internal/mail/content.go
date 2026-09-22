package mail

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/yuin/goldmark"
)

// markdown is a package-level goldmark instance to avoid re-allocating the
// parser/renderer on every draft body conversion.
var markdown = goldmark.New()

type draftContentObserver interface {
	ContentRendered()
}

type preparedDraftContent struct {
	Format      DraftBodyFormat
	Source      string
	Plain       string
	HTML        string
	Diagnostics []ContentDiagnostic
}

func prepareDraftContent(format DraftBodyFormat, source string) (preparedDraftContent, error) {
	return prepareDraftContentWithObserver(context.Background(), format, source, nil)
}

func prepareDraftContentWithObserver(
	ctx context.Context,
	format DraftBodyFormat,
	source string,
	observer draftContentObserver,
) (preparedDraftContent, error) {
	if err := ctx.Err(); err != nil {
		return preparedDraftContent{}, err
	}
	if observer != nil {
		observer.ContentRendered()
	}
	if err := ctx.Err(); err != nil {
		return preparedDraftContent{}, err
	}
	if format == "" {
		format = DraftBodyPlain
	}
	if len(source) > MaximumDraftBodyBytes {
		return preparedDraftContent{}, validationError("draft body exceeds 4 MiB")
	}
	switch format {
	case DraftBodyPlain:
		return preparedDraftContent{Format: format, Plain: source}, nil
	case DraftBodyMarkdown:
		return renderMarkdownContent(ctx, source)
	case DraftBodyHTML:
		return canonicalRichContent(ctx, format, source, strings.NewReader(source))
	default:
		return preparedDraftContent{}, validationError("draft body format must be plain, markdown, or html")
	}
}

func validateStoredDraftContent(draft Draft) error {
	return validateStoredDraftContentWithObserver(&draft, nil)
}

func validateStoredDraftContentWithObserver(draft *Draft, observer draftContentObserver) error {
	if draft == nil {
		return validationError("stored draft is missing")
	}
	switch draft.BodyFormat {
	case DraftBodyPlain:
		if draft.BodySource != "" || draft.BodyHTML != "" || len(draft.ContentDiagnostics) > 0 {
			return validationError("plain draft contains unexpected rich content")
		}
	case DraftBodyMarkdown, DraftBodyHTML:
		if draft.BodySource == "" && (draft.Body != "" || draft.BodyHTML != "") {
			return validationError("rich draft is missing its source body")
		}
		prepared, err := prepareDraftContentWithObserver(context.Background(), draft.BodyFormat, draft.BodySource, observer)
		if err != nil {
			return err
		}
		if prepared.Plain != draft.Body || prepared.HTML != draft.BodyHTML {
			return validationError("stored rich draft does not match its canonical rendering")
		}
		// Drafts written before diagnostics existed remain readable. New writes
		// always carry the computed values, and a present value is integrity
		// checked against the canonical transformation.
		if draft.ContentDiagnostics != nil &&
			!contentDiagnosticsEqual(prepared.Diagnostics, draft.ContentDiagnostics) {
			return validationError("stored rich draft diagnostics do not match its canonical rendering")
		}
		draft.ContentDiagnostics = prepared.Diagnostics
	default:
		return validationError("stored draft has an unsupported body format")
	}
	return validateStoredDraftLimits(*draft)
}

// validateStoredDraftContentStructuralWithObserver checks stored shape,
// format, and limits without re-rendering rich bodies. A draft whose stored
// diagnostics are absent (legacy layout) still runs the canonical pass so
// display receives computed diagnostics; a present diagnostics value is
// trusted on read paths and re-verified canonically on mutation gates.
func validateStoredDraftContentStructuralWithObserver(draft *Draft, observer draftContentObserver) error {
	if draft == nil {
		return validationError("stored draft is missing")
	}
	switch draft.BodyFormat {
	case DraftBodyPlain:
		if draft.BodySource != "" || draft.BodyHTML != "" || len(draft.ContentDiagnostics) > 0 {
			return validationError("plain draft contains unexpected rich content")
		}
	case DraftBodyMarkdown, DraftBodyHTML:
		if draft.BodySource == "" && (draft.Body != "" || draft.BodyHTML != "") {
			return validationError("rich draft is missing its source body")
		}
		if len(draft.BodySource) > MaximumDraftBodyBytes {
			return validationError("draft body exceeds 4 MiB")
		}
		if draft.ContentDiagnostics == nil {
			prepared, err := prepareDraftContentWithObserver(
				context.Background(), draft.BodyFormat, draft.BodySource, observer,
			)
			if err != nil {
				return err
			}
			if prepared.Plain != draft.Body || prepared.HTML != draft.BodyHTML {
				return validationError("stored rich draft does not match its canonical rendering")
			}
			draft.ContentDiagnostics = prepared.Diagnostics
		}
	default:
		return validationError("stored draft has an unsupported body format")
	}
	return validateStoredDraftLimits(*draft)
}

func renderMarkdownContent(ctx context.Context, source string) (preparedDraftContent, error) {
	rendered := draftHTMLWriter{ctx: ctx}
	if err := markdown.Convert([]byte(source), &rendered); err != nil {
		return preparedDraftContent{}, fmt.Errorf("render Markdown body: %w", err)
	}
	return canonicalRichContent(ctx, DraftBodyMarkdown, source, strings.NewReader(rendered.value.String()))
}

func canonicalRichContent(ctx context.Context, format DraftBodyFormat, source string, input io.Reader) (preparedDraftContent, error) {
	body, diagnostics, err := sanitizeEmailHTML(ctx, input)
	if err != nil {
		return preparedDraftContent{}, err
	}
	sanitized, err := renderSanitizedHTML(ctx, body)
	if err != nil {
		return preparedDraftContent{}, err
	}
	plain, err := renderPreparedDraftText(ctx, body, sanitized)
	if err != nil {
		return preparedDraftContent{}, err
	}
	if len(plain) > MaximumDraftBodyBytes {
		return preparedDraftContent{}, validationError("plain-text draft body exceeds 4 MiB")
	}
	return preparedDraftContent{
		Format: format, Source: source, Plain: plain, HTML: sanitized, Diagnostics: diagnostics,
	}, nil
}

type contentDiagnosticCollector struct {
	values []ContentDiagnostic
	seen   map[string]struct{}
}

func (collector *contentDiagnosticCollector) add(code, element, attribute string) {
	if collector == nil || code == "" {
		return
	}
	if collector.seen == nil {
		collector.seen = make(map[string]struct{})
	}
	key := code + "\x00" + element + "\x00" + attribute
	if _, exists := collector.seen[key]; exists {
		return
	}
	collector.seen[key] = struct{}{}
	collector.values = append(collector.values, ContentDiagnostic{
		Code: code, Element: element, Attribute: attribute,
	})
}

func contentDiagnosticsEqual(left, right []ContentDiagnostic) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
