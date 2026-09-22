package mailstore

import (
	"mime"
	"strconv"
	"strings"
)

func mimeStringBytes(values ...string) int64 {
	var amount int64
	for _, value := range values {
		length := int64(len(value))
		if length > maximumInt64-amount {
			return maximumInt64
		}
		amount += length
	}
	return amount
}

// parseAppleContentLength reads the decoded-length hint from Mail's
// X-Apple-Content-Length header for buffer pre-sizing.
func parseAppleContentLength(value string) (int64, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false
	}
	length, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || length < 0 {
		return 0, false
	}
	return length, true
}

func missingAppleContent(value string, available int64, partial bool) bool {
	if !partial {
		return false
	}
	expected, ok := parseAppleContentLength(value)
	return ok && expected > available
}

func mimePartID(path []int) string {
	if len(path) == 0 {
		return "1"
	}
	var buf []byte
	for index, component := range path {
		if index > 0 {
			buf = append(buf, '.')
		}
		buf = strconv.AppendInt(buf, int64(component+1), 10)
	}
	return string(buf)
}

func markMissingPart(document *mimeDocument, identifier string) {
	document.Complete = false
	for _, existing := range document.MissingParts {
		if existing == identifier {
			return
		}
	}
	if document.budget != nil && !document.budget.reserveMetadata(int64(len(identifier))) {
		markMIMEBudgetExceeded(document, document.budget.error())
		return
	}
	appendMissingPart(document, identifier)
}

func guessedMIMEType(name string) *string {
	mediaType := mime.TypeByExtension(strings.ToLower(filepathExtension(name)))
	if mediaType == "" {
		return nil
	}
	if baseType, _, err := mime.ParseMediaType(mediaType); err == nil {
		mediaType = baseType
	}
	return &mediaType
}

func filepathExtension(name string) string {
	index := strings.LastIndexByte(name, '.')
	if index < 0 {
		return ""
	}
	return name[index:]
}
