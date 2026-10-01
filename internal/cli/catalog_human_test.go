package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

type catalogTerminalWriter struct {
	bytes.Buffer
	fd uintptr
}

func (w *catalogTerminalWriter) Fd() uintptr { return w.fd }

func TestHumanCatalogContinuation(t *testing.T) {
	for _, catalog := range []struct {
		family string
		prefix string
	}{
		{"accounts", "account"},
		{"mailboxes", "mailbox"},
	} {
		for _, terminal := range []bool{false, true} {
			for _, count := range []int{0, 3} {
				t.Run(fmt.Sprintf("%s/terminal=%t/count=%d", catalog.family, terminal, count), func(t *testing.T) {
					service := mail.NewService(newCatalogPagingGateway(count))
					var capture bytes.Buffer
					var writer io.Writer = &capture
					if terminal {
						_, slave := openEditorTestTerminal(t)
						tty := &catalogTerminalWriter{fd: slave.Fd()}
						writer = tty
					}
					cursor := ""
					for index := 0; index < max(1, count); index++ {
						args := []string{catalog.family, "list", "--limit", "1"}
						if cursor != "" {
							args = append(args, "--cursor", cursor)
						}
						jsonResult, code := runListJSON(t, service, append(append([]string(nil), args...), "--json"))
						if code != 0 {
							t.Fatalf("JSON page failed: %+v", jsonResult)
						}
						cursor = responsePage(t, jsonResult).NextCursor
						var stderr bytes.Buffer
						if code := Run(context.Background(), service, args, writer, &stderr); code != 0 || stderr.Len() != 0 {
							t.Fatalf("human page failed: code=%d stderr=%q", code, stderr.String())
						}
						output := capture.String()
						if tty, ok := writer.(*catalogTerminalWriter); ok {
							output = tty.String()
							tty.Reset()
						}
						capture.Reset()
						if terminal != strings.Contains(output, "REF") {
							t.Fatalf("wrong output branch: %q", output)
						}
						for row := 0; row < count; row++ {
							ref := fmt.Sprintf("%s-%03d", catalog.prefix, row)
							if strings.Contains(output, ref) != (row == index) {
								t.Fatalf("wrong row on page %d: %q", index, output)
							}
						}
						marker := "next_cursor\t"
						if terminal {
							marker = "Next cursor: "
						}
						if cursor == "" {
							if strings.Contains(output, marker) {
								t.Fatalf("terminal page claims continuation: %q", output)
							}
						} else if strings.Count(output, marker+cursor+"\n") != 1 {
							t.Fatalf("missing or duplicated usable cursor on page %d: %q", index, output)
						}
					}
				})
			}
		}
	}
}

func TestHumanCatalogRejectsInvalidPaging(t *testing.T) {
	service := mail.NewService(newCatalogPagingGateway(3))
	first, code := runListJSON(t, service, []string{"accounts", "list", "--limit", "1", "--json"})
	if code != 0 {
		t.Fatal("cannot obtain account cursor")
	}
	cursor := responsePage(t, first).NextCursor
	for _, args := range [][]string{
		{"accounts", "list", "--limit", "0"},
		{"mailboxes", "list", "--limit", "201"},
		{"accounts", "list", "--cursor", "invalid"},
		{"mailboxes", "list", "--cursor", cursor},
	} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), service, args, &stdout, &stderr); code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("invalid paging accepted: code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
			}
		})
	}
}
