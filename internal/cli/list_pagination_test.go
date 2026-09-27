package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

type catalogPagingGateway struct {
	testGateway
	accounts      []mail.Account
	mailboxes     []mail.Mailbox
	message       mail.Message
	messageLimit  int
	searchLimit   int
	threadRequest mail.MessageThreadRequest
}

func (g *catalogPagingGateway) ListAccounts(context.Context) ([]mail.Account, error) {
	return g.accounts, nil
}

func (g *catalogPagingGateway) ListMailboxes(_ context.Context, request mail.ListMailboxesRequest) ([]mail.Mailbox, error) {
	values := make([]mail.Mailbox, 0, len(g.mailboxes))
	for _, mailbox := range g.mailboxes {
		if request.AccountRef == "" || mailbox.AccountRef == request.AccountRef {
			values = append(values, mailbox)
		}
	}
	return values, nil
}

func (g *catalogPagingGateway) ListMessages(_ context.Context, request mail.ListMessagesRequest) (mail.MessagePage, error) {
	g.messageLimit = request.Limit
	return mail.MessagePage{Messages: []mail.MessageSummary{{Ref: "message-result"}}}, nil
}

func (g *catalogPagingGateway) SearchMessages(_ context.Context, query mail.PreparedQuery) (mail.SearchPage, error) {
	g.searchLimit = query.Query.Limit
	return mail.SearchPage{
		Messages: []mail.SearchMessage{{Summary: mail.MessageSummary{Ref: "search-result"}}},
		Coverage: mail.SearchCoverage{Backend: "test", Complete: true, CandidateMessagesExact: true},
	}, nil
}

func (g *catalogPagingGateway) MessageThread(_ context.Context, request mail.MessageThreadRequest) (mail.MessageThread, error) {
	g.threadRequest = request
	return mail.MessageThread{Ref: request.Ref, Messages: []mail.MessageSummary{{Ref: "thread-result"}}}, nil
}

func (g *catalogPagingGateway) GetMessage(context.Context, string) (mail.Message, error) {
	return g.message, nil
}

func newCatalogPagingGateway(count int) *catalogPagingGateway {
	gateway := &catalogPagingGateway{message: mail.Message{
		Summary: mail.MessageSummary{Ref: "msg_ref"}, ContentSource: "local", ContentComplete: true,
	}}
	for index := range count {
		gateway.accounts = append(gateway.accounts, mail.Account{
			Ref: fmt.Sprintf("account-%03d", index), Name: "same display name", State: "ok",
		})
		gateway.mailboxes = append(gateway.mailboxes, mail.Mailbox{
			Ref: fmt.Sprintf("mailbox-%03d", index), AccountRef: "account_scope", Name: "same display name",
			Path: []string{"same display path"},
		})
		gateway.message.Attachments = append(gateway.message.Attachments, mail.Attachment{
			ID: fmt.Sprintf("part.%03d", index), Name: "same display name",
		})
	}
	return gateway
}

func runListJSON(t *testing.T, service *mail.Service, args []string) (envelope, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, args, &stdout, &stderr)
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode %v response (code %d): %v; stdout=%s stderr=%s", args, code, err, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr for %v: %s", args, stderr.String())
	}
	return response, code
}

func responsePage(t *testing.T, response envelope) catalogPageMetadata {
	t.Helper()
	if response.Data.Page == nil {
		t.Fatal("list response omitted data.page")
	}
	var page catalogPageMetadata
	if err := json.Unmarshal(*response.Data.Page, &page); err != nil {
		t.Fatalf("decode data.page: %v", err)
	}
	return page
}

func TestCatalogListsPageStableIdentitiesAboveTwoHundred(t *testing.T) {
	t.Run("accounts", func(t *testing.T) {
		gateway := newCatalogPagingGateway(205)
		gateway.accounts[204].State = "degraded"
		service := mail.NewService(gateway)
		first, code := runListJSON(t, service, []string{"accounts", "list", "--limit", "200", "--json"})
		if code != 0 || first.Data.Accounts == nil || len(*first.Data.Accounts) != 200 {
			t.Fatalf("first account page: code=%d response=%+v", code, first.Data)
		}
		if first.Data.Complete == nil || *first.Data.Complete {
			t.Fatal("account completeness was calculated from the page instead of the full catalog")
		}
		cursor := responsePage(t, first).NextCursor
		if cursor == "" || (*first.Data.Accounts)[0].Name != "same display name" {
			t.Fatalf("account cursor or duplicate display fixture is missing: %q", cursor)
		}
		gateway.accounts[0].Name = "mutable account metadata"
		second, code := runListJSON(t, service, []string{"accounts", "list", "--limit", "5", "--cursor", cursor, "--json"})
		if code != 0 || second.Data.Accounts == nil || len(*second.Data.Accounts) != 5 ||
			(*second.Data.Accounts)[0].Ref != "account-200" || responsePage(t, second).NextCursor != "" {
			t.Fatalf("second account page: code=%d response=%+v", code, second.Data)
		}
	})

	t.Run("mailboxes", func(t *testing.T) {
		gateway := newCatalogPagingGateway(205)
		service := mail.NewService(gateway)
		first, code := runListJSON(t, service, []string{"mailboxes", "list", "--account", "account_scope", "--limit", "200", "--json"})
		if code != 0 || first.Data.Mailboxes == nil || len(*first.Data.Mailboxes) != 200 {
			t.Fatalf("first mailbox page: code=%d response=%+v", code, first.Data)
		}
		cursor := responsePage(t, first).NextCursor
		gateway.mailboxes[0].UnreadCount++
		second, code := runListJSON(t, service, []string{"mailboxes", "list", "--account", "account_scope", "--limit", "5", "--cursor", cursor, "--json"})
		if code != 0 || second.Data.Mailboxes == nil || len(*second.Data.Mailboxes) != 5 ||
			(*second.Data.Mailboxes)[0].Ref != "mailbox-200" || responsePage(t, second).NextCursor != "" {
			t.Fatalf("second mailbox page: code=%d response=%+v", code, second.Data)
		}
		wrongScope, code := runListJSON(t, service, []string{"mailboxes", "list", "--account", "another_account", "--cursor", cursor, "--json"})
		if code != 2 || wrongScope.Error == nil || wrongScope.Error.Code != "invalid_cursor" ||
			wrongScope.Error.Guidance == nil || wrongScope.Error.Guidance.Recovery.Instruction != "Restart this list without --cursor." {
			t.Fatalf("mailbox cursor escaped its account scope: code=%d error=%+v", code, wrongScope.Error)
		}
	})

	t.Run("attachments", func(t *testing.T) {
		gateway := newCatalogPagingGateway(205)
		service := mail.NewService(gateway)
		first, code := runListJSON(t, service, []string{"attachments", "list", "--ref", "msg_ref", "--limit", "200", "--json"})
		if code != 0 || first.Data.Attachments == nil || len(*first.Data.Attachments) != 200 {
			t.Fatalf("first attachment page: code=%d response=%+v", code, first.Data)
		}
		cursor := responsePage(t, first).NextCursor
		wrongScope, code := runListJSON(t, service, []string{"attachments", "list", "--ref", "another_message", "--cursor", cursor, "--json"})
		if code != 2 || wrongScope.Error == nil || wrongScope.Error.Code != "invalid_cursor" {
			t.Fatalf("attachment cursor escaped its message scope: code=%d error=%+v", code, wrongScope.Error)
		}
		second, code := runListJSON(t, service, []string{"attachments", "list", "--ref", "msg_ref", "--limit", "5", "--cursor", cursor, "--json"})
		if code != 0 || second.Data.Attachments == nil || len(*second.Data.Attachments) != 5 ||
			(*second.Data.Attachments)[0].ID != "part.200" || responsePage(t, second).NextCursor != "" {
			t.Fatalf("second attachment page: code=%d response=%+v", code, second.Data)
		}
	})
}

func TestCatalogCursorBindsCommand(t *testing.T) {
	service := mail.NewService(newCatalogPagingGateway(205))
	first, code := runListJSON(t, service, []string{"accounts", "list", "--limit", "1", "--json"})
	if code != 0 {
		t.Fatalf("first page failed: %+v", first.Error)
	}
	cursor := responsePage(t, first).NextCursor
	response, code := runListJSON(t, service, []string{"mailboxes", "list", "--cursor", cursor, "--json"})
	if code != 2 || response.Error == nil || response.Error.Code != "invalid_cursor" {
		t.Fatalf("account cursor was accepted by mailboxes.list: code=%d error=%+v", code, response.Error)
	}
}

func TestCatalogCursorRejectsChangedIdentityOrder(t *testing.T) {
	gateway := newCatalogPagingGateway(205)
	service := mail.NewService(gateway)
	first, code := runListJSON(t, service, []string{"accounts", "list", "--limit", "200", "--json"})
	if code != 0 {
		t.Fatalf("first page failed: %+v", first.Error)
	}
	cursor := responsePage(t, first).NextCursor
	gateway.accounts[0], gateway.accounts[1] = gateway.accounts[1], gateway.accounts[0]
	changed, code := runListJSON(t, service, []string{"accounts", "list", "--limit", "200", "--cursor", cursor, "--json"})
	if code != 2 || changed.Error == nil || changed.Error.Code != "invalid_cursor" {
		t.Fatalf("changed identity order was accepted: code=%d response=%+v", code, changed)
	}
}

func TestListCommandsSharePageBoundsAndDefaults(t *testing.T) {
	commands := []struct {
		name      string
		commandID string
		args      []string
	}{
		{name: "accounts", commandID: "accounts.list", args: []string{"accounts", "list"}},
		{name: "mailboxes", commandID: "mailboxes.list", args: []string{"mailboxes", "list"}},
		{name: "attachments", commandID: "attachments.list", args: []string{"attachments", "list", "--ref", "msg_ref"}},
		{name: "messages", commandID: "messages.list", args: []string{"messages", "list", "--mailbox", "mailbox-000"}},
		{name: "thread", commandID: "messages.thread", args: []string{"messages", "thread", "--ref", "msg_ref"}},
		{name: "filter", commandID: "messages.filter", args: []string{"messages", "filter", "--read", "true"}},
		{name: "search", commandID: "messages.search", args: []string{"messages", "search", "--query", "needle"}},
		{name: "drafts", commandID: "drafts.list", args: []string{"drafts", "list"}},
	}
	for _, test := range commands {
		t.Run(test.name, func(t *testing.T) {
			flags := schemaFlagsByName(decodeTestCommandSchema(t, schemaForCommand(test.commandID)))
			limitFlag, exists := flags["--limit"]
			if !exists || limitFlag.Default != strconv.Itoa(mail.DefaultPageLimit) ||
				limitFlag.Minimum == nil || *limitFlag.Minimum != 1 ||
				limitFlag.Maximum == nil || *limitFlag.Maximum != mail.MaximumPageLimit ||
				!strings.Contains(limitFlag.Description, "1-200") {
				t.Fatalf("%s --limit schema = %+v", test.commandID, limitFlag)
			}
			maxBytesFlag, exists := flags["--max-bytes"]
			if !exists || maxBytesFlag.Default != strconv.FormatInt(defaultJSONOutputBytes, 10) ||
				maxBytesFlag.Minimum == nil || *maxBytesFlag.Minimum != 1 ||
				maxBytesFlag.Maximum == nil || *maxBytesFlag.Maximum != maximumJSONOutputBytes {
				t.Fatalf("%s --max-bytes schema = %+v", test.commandID, maxBytesFlag)
			}
			for _, limit := range []struct {
				value string
				want  int
			}{{value: "", want: mail.DefaultPageLimit}, {value: "1", want: 1}, {value: "200", want: mail.MaximumPageLimit}} {
				gateway := newCatalogPagingGateway(205)
				service := mail.NewService(gateway)
				args := append([]string(nil), test.args...)
				if limit.value != "" {
					args = append(args, "--limit", limit.value)
				}
				args = append(args, "--json")
				response, code := runListJSON(t, service, args)
				if code != 0 || !response.OK {
					t.Fatalf("limit %q failed: code=%d error=%+v", limit.value, code, response.Error)
				}
				var observed int
				switch test.name {
				case "accounts", "mailboxes", "attachments", "drafts":
					observed = responsePage(t, response).Limit
				case "messages":
					observed = gateway.messageLimit
				case "thread":
					observed = gateway.threadRequest.Limit
				case "filter", "search":
					observed = gateway.searchLimit
				}
				if observed != limit.want {
					t.Fatalf("observed page limit = %d, want %d", observed, limit.want)
				}
			}
			for _, invalid := range []string{"0", "201"} {
				gateway := newCatalogPagingGateway(1)
				service := mail.NewService(gateway)
				args := append(append([]string(nil), test.args...), "--limit", invalid, "--json")
				response, code := runListJSON(t, service, args)
				if code != 2 || response.Error == nil || response.Error.Code != "invalid_argument" {
					t.Fatalf("invalid limit %s was accepted: code=%d error=%+v", invalid, code, response.Error)
				}
			}
		})
	}
}

func TestListOutputOverflowReturnsNoRowsOrContinuation(t *testing.T) {
	commands := []struct {
		name string
		args []string
	}{
		{name: "accounts", args: []string{"accounts", "list"}},
		{name: "mailboxes", args: []string{"mailboxes", "list"}},
		{name: "attachments", args: []string{"attachments", "list", "--ref", "msg_ref"}},
		{name: "messages", args: []string{"messages", "list", "--mailbox", "mailbox-000"}},
		{name: "thread", args: []string{"messages", "thread", "--ref", "msg_ref"}},
		{name: "filter", args: []string{"messages", "filter", "--read", "true"}},
		{name: "search", args: []string{"messages", "search", "--query", "needle"}},
	}
	for _, test := range commands {
		t.Run(test.name, func(t *testing.T) {
			service := mail.NewService(newCatalogPagingGateway(3))
			args := append(append([]string(nil), test.args...), "--max-bytes", "1", "--json")
			response, code := runListJSON(t, service, args)
			if code != 1 || response.OK || response.Error == nil || response.Error.Code != "output_too_large" ||
				response.Data.Page != nil || strings.Contains(string(mustJSON(t, response)), "next_cursor") ||
				response.Error.Guidance == nil || response.Error.Guidance.Recovery.Action != mail.RecoveryCorrect ||
				!strings.Contains(response.Error.Guidance.Recovery.Instruction, "same incoming --cursor") {
				t.Fatalf("overflow returned a partial page or weak recovery: code=%d response=%+v", code, response)
			}
			switch test.name {
			case "accounts":
				if response.Data.Accounts != nil {
					t.Fatal("overflow returned account rows")
				}
			case "mailboxes":
				if response.Data.Mailboxes != nil {
					t.Fatal("overflow returned mailbox rows")
				}
			case "attachments":
				if response.Data.Attachments != nil {
					t.Fatal("overflow returned attachment rows")
				}
			case "thread":
				if response.Data.Thread != nil {
					t.Fatal("overflow returned thread rows")
				}
			}
			if response.Data.RequiredBytes == nil || *response.Data.RequiredBytes <= 1 ||
				response.Data.LimitBytes == nil || *response.Data.LimitBytes != 1 || response.Data.Measured != string(outputSizeExact) {
				t.Fatalf("overflow size evidence = %+v", response.Data)
			}
		})
	}
}

func TestListOverflowRecoveryCanRetrySameIncomingCursor(t *testing.T) {
	gateway := newCatalogPagingGateway(205)
	service := mail.NewService(gateway)
	first, code := runListJSON(t, service, []string{"accounts", "list", "--limit", "1", "--json"})
	if code != 0 {
		t.Fatalf("first page failed: %+v", first.Error)
	}
	cursor := responsePage(t, first).NextCursor
	args := []string{"accounts", "list", "--limit", "1", "--cursor", cursor, "--max-bytes", "1", "--json"}
	tooLarge, code := runListJSON(t, service, args)
	if code != 1 || tooLarge.Error == nil || tooLarge.Error.Code != "output_too_large" || tooLarge.Data.Page != nil {
		t.Fatalf("oversized continuation retained its page: code=%d response=%+v", code, tooLarge)
	}
	if tooLarge.Data.RequiredBytes == nil {
		t.Fatal("oversized continuation omitted required_bytes")
	}
	args[len(args)-2] = strconv.FormatInt(*tooLarge.Data.RequiredBytes, 10)
	retried, code := runListJSON(t, service, args)
	if code != 0 || retried.Data.Accounts == nil || len(*retried.Data.Accounts) != 1 ||
		(*retried.Data.Accounts)[0].Ref != "account-001" || responsePage(t, retried).NextCursor == "" {
		t.Fatalf("same-cursor exact-budget retry failed: code=%d response=%+v", code, retried.Data)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal test response: %v", err)
	}
	return payload
}
