package mailstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

const testAccountID = "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"

func TestStoreListAndSearchUsesLabelsAndStatelessEMLX(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")

	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(page.Messages) != 3 || page.Messages[0].Subject != "Quarterly Report" {
		t.Fatalf("ListMessages() = %#v", page)
	}

	metadataQuery, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Subject: "Quarterly", Limit: 10,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	metadata, err := store.SearchMessages(context.Background(), metadataQuery)
	if err != nil {
		t.Fatalf("SearchMessages(metadata) error = %v", err)
	}
	if len(metadata.Messages) != 1 || metadata.Coverage.Backend != "envelope_sql" ||
		metadata.Coverage.CandidateMessages != 1 || !metadata.Coverage.CandidateMessagesExact ||
		!metadata.Coverage.Complete ||
		metadata.Coverage.Consistency != mail.SearchConsistencyBestEffort ||
		metadata.Coverage.IndexRevision == "" {
		t.Fatalf("metadata search = %#v", metadata)
	}

	bodyQuery, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 1,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	bodyPage, err := store.SearchMessages(context.Background(), bodyQuery)
	if err != nil {
		t.Fatalf("SearchMessages(body) error = %v", err)
	}
	if len(bodyPage.Messages) != 1 || bodyPage.Messages[0].Summary.Subject != "Quarterly Report" ||
		bodyPage.NextCursor == "" || bodyPage.Coverage.Complete {
		t.Fatalf("body search page = %#v", bodyPage)
	}
	firstCursor, err := mail.DecodeSearchCursor(bodyPage.NextCursor, bodyQuery.Fingerprint)
	if err != nil {
		t.Fatalf("DecodeSearchCursor(first) error = %v", err)
	}
	if firstCursor.RowID != 101 || firstCursor.IndexRevision != bodyPage.Coverage.IndexRevision {
		t.Fatalf("first body cursor = %+v, coverage = %+v", firstCursor, bodyPage.Coverage)
	}

	nextQuery, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 1, Cursor: bodyPage.NextCursor,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(next) error = %v", err)
	}
	nextPage, err := store.SearchMessages(context.Background(), nextQuery)
	if err != nil {
		t.Fatalf("SearchMessages(next) error = %v", err)
	}
	if len(nextPage.Messages) != 1 || nextPage.Messages[0].Summary.Subject != "Status Update" {
		t.Fatalf("next body search page = %#v", nextPage)
	}
	nextCursor, err := mail.DecodeSearchCursor(nextPage.NextCursor, nextQuery.Fingerprint)
	if err != nil {
		t.Fatalf("DecodeSearchCursor(next) error = %v", err)
	}
	if nextCursor.RowID != 102 {
		t.Fatalf("next body cursor row = %d, want last examined row 102", nextCursor.RowID)
	}
}

func TestMetadataSearchUsesBoundedPageAndOptionalCount(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t, 600)
	closeTestResource(t, store, "test store")
	prepared, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Subject: "Status", Limit: 1,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), prepared)
	if err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
	if len(page.Messages) != 1 || page.NextCursor == "" {
		t.Fatalf("metadata page = %#v, want one result and a cursor", page)
	}
	if page.Coverage.CandidateMessages != 2 || page.Coverage.CandidateMessagesExact || !page.Coverage.Complete {
		t.Fatalf("metadata coverage = %#v, want a two-row lower bound", page.Coverage)
	}
	if got := store.searchMetadataRowsLoaded.Load(); got != 2 {
		t.Fatalf("metadata rows loaded = %d, want page plus one continuation row", got)
	}
	if got := store.searchCandidateCountQueries.Load(); got != 0 {
		t.Fatalf("candidate count queries = %d, want no default count query", got)
	}
}

func TestMetadataSearchExactCountHonorsCursorScope(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t, 600)
	closeTestResource(t, store, "test store")
	first, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Subject: "Status", Limit: 1, MaxMessages: 1000,
		ExactCount: true,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(first) error = %v", err)
	}
	firstPage, err := store.SearchMessages(context.Background(), first)
	if err != nil || firstPage.NextCursor == "" {
		t.Fatalf("SearchMessages(first) = %#v, error = %v", firstPage, err)
	}
	next, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Subject: "Status", Limit: 25, MaxMessages: 1000,
		ExactCount: true, Cursor: firstPage.NextCursor,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(next) error = %v", err)
	}
	nextPage, err := store.SearchMessages(context.Background(), next)
	if err != nil {
		t.Fatalf("SearchMessages(next) error = %v", err)
	}
	plan, empty, err := store.prepareSearchPlan(context.Background(), next)
	if err != nil || empty {
		t.Fatalf("prepareSearchPlan(reference) = empty %t, error = %v", empty, err)
	}
	var referenceTotal int
	referenceQuery := plan.prefixSQL + "SELECT count(*) " + plan.fromWhereSQL
	if err := store.database.QueryRowContext(context.Background(), referenceQuery, plan.arguments...).Scan(&referenceTotal); err != nil {
		t.Fatalf("reference candidate count: %v", err)
	}
	if len(nextPage.Messages) != 25 || nextPage.NextCursor == "" {
		t.Fatalf("metadata exact page = %#v, want 25 results and a cursor", nextPage)
	}
	if nextPage.Coverage.CandidateMessages != referenceTotal || referenceTotal != 600 ||
		!nextPage.Coverage.CandidateMessagesExact || !nextPage.Coverage.Complete {
		t.Fatalf("metadata exact coverage = %#v, want reference total %d (600) after cursor", nextPage.Coverage, referenceTotal)
	}
	if got := store.searchCandidateCountQueries.Load(); got != 2 {
		t.Fatalf("candidate count queries = %d, want one explicit count per page", got)
	}
}

func TestMetadataSearchExactCountRejectsBoundExceeded(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t, 600)
	closeTestResource(t, store, "test store")
	prepared, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Subject: "Status", Limit: 1, MaxMessages: 100,
		ExactCount: true,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	if _, err := store.SearchMessages(context.Background(), prepared); errorCodeForTest(err) != "search_count_limit_exceeded" {
		t.Fatalf("SearchMessages() error = %v, want search_count_limit_exceeded", err)
	}
	if got := store.searchCandidateCountQueries.Load(); got != 1 {
		t.Fatalf("candidate count queries = %d, want one explicit count", got)
	}
}

func TestMetadataSearchPaginationPreservesEqualAndNullDates(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t, 4)
	closeTestResource(t, store, "test store")
	updateFixtureMessage(t, store, `UPDATE messages SET date_received = 900 WHERE ROWID IN (104, 105)`)
	updateFixtureMessage(t, store, `UPDATE messages SET date_received = NULL WHERE ROWID IN (106, 107)`)

	var rowIDs []string
	cursor := ""
	for pageNumber, limit := range []int{2, 1, 4} {
		prepared, err := mail.PrepareQuery(mail.Query{
			MailboxRef: inboxRef, Subject: "Status", Limit: limit, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("PrepareQuery(page %d) error = %v", pageNumber, err)
		}
		page, err := store.SearchMessages(context.Background(), prepared)
		if err != nil {
			t.Fatalf("SearchMessages(page %d) error = %v", pageNumber, err)
		}
		for _, message := range page.Messages {
			ref, err := mailref.DecodeMessage(message.Summary.Ref)
			if err != nil {
				t.Fatalf("DecodeMessage(page %d) error = %v", pageNumber, err)
			}
			rowIDs = append(rowIDs, ref.LibraryID)
		}
		cursor = page.NextCursor
	}
	assertSearchRowIDs(t, rowIDs, []string{"105", "104", "102", "107", "106"})
}

func TestOpenSearchCandidateRejectsStaleStoreIdentity(t *testing.T) {
	tests := []struct {
		name   string
		update string
	}{
		{
			name:   "message identity replaced",
			update: `UPDATE messages SET message_id = 9001 WHERE ROWID = 101`,
		},
		{
			name:   "mailbox moved",
			update: `UPDATE messages SET mailbox = 1 WHERE ROWID = 101`,
		},
		{
			name:   "subject replaced",
			update: `UPDATE subjects SET subject = 'Replaced' WHERE ROWID = 1`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, inboxRef := newSearchFixture(t)
			closeTestResource(t, store, "test store")
			page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
				MailboxRef: inboxRef, Limit: 3,
			})
			if err != nil {
				t.Fatalf("ListMessages() error = %v", err)
			}
			messageRef := messageRefWithSubject(t, page.Messages, "Quarterly Report")
			resolved, err := store.resolveMessage(context.Background(), messageRef)
			if err != nil {
				t.Fatalf("resolveMessage() error = %v", err)
			}
			updateFixtureMessage(t, store, test.update)
			source, unavailable, err := store.openSearchCandidate(context.Background(), resolved.Record)
			if source != nil {
				if closeErr := source.Close(); closeErr != nil {
					t.Fatalf("close unexpected source: %v", closeErr)
				}
				t.Fatal("openSearchCandidate() returned source for stale identity")
			}
			if unavailable.missing {
				t.Fatal("openSearchCandidate() marked stale identity as missing source")
			}
			if errorCodeForTest(err) != "stale_reference" {
				t.Fatalf("openSearchCandidate() error = %v, want stale_reference", err)
			}
		})
	}
}

func TestBodySearchReportsByteLimit(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	query, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 10, MaxBytes: 1,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), query)
	if err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
	if page.Coverage.Complete || page.Coverage.ScannedBytes != 0 {
		t.Fatalf("limited coverage = %#v", page.Coverage)
	}
}

func TestBodySearchStopsAtFirstCandidateOutsideByteBudget(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	query, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 10, MaxBytes: 256,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), query)
	if err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
	if len(page.Messages) != 0 || page.Coverage.Complete || page.Coverage.ScannedBytes != 0 {
		t.Fatalf("byte-limited ordered search = %#v", page)
	}
}

func TestAttachmentFilterUsesMIMEWhenEnvelopeCatalogLags(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	attachmentBytes := []byte("catalog-lag attachment")
	writeFixtureEMLX(
		t,
		store,
		101,
		"imap://"+testAccountID+"/%5BGmail%5D/All",
		sentFixtureSource(101, sentMessageFixture{
			Body: "needle alpha", RecipientTypes: []int{0},
			Attachments: []fixtureAttachment{{Name: "report.bin", Content: attachmentBytes}},
		}),
	)
	updateFixtureMessage(t, store, `DELETE FROM attachments WHERE message = 101`)

	hasAttachment := true
	withQuery, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, HasAttachment: &hasAttachment, Limit: 10,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(with attachment) error = %v", err)
	}
	withPage, err := store.SearchMessages(context.Background(), withQuery)
	if err != nil || len(withPage.Messages) != 1 ||
		withPage.Messages[0].Summary.Subject != "Quarterly Report" ||
		withPage.Messages[0].Summary.AttachmentCount != 1 ||
		withPage.Coverage.Backend != "emlx_stream" || !withPage.Coverage.Complete {
		t.Fatalf("attachment=true page = %#v, error = %v", withPage, err)
	}

	hasAttachment = false
	withoutQuery, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, HasAttachment: &hasAttachment, Limit: 10,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(without attachment) error = %v", err)
	}
	withoutPage, err := store.SearchMessages(context.Background(), withoutQuery)
	if err != nil || len(withoutPage.Messages) != 2 ||
		withoutPage.Coverage.Backend != "emlx_stream" || !withoutPage.Coverage.Complete {
		t.Fatalf("attachment=false page = %#v, error = %v", withoutPage, err)
	}
}

func TestAttachmentFilterUsesPositiveCatalogWhenSourceIsMissing(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	location, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
	if err != nil {
		t.Fatalf("parseMailboxURL() error = %v", err)
	}
	base, err := store.messageBasePath(location, 101)
	if err != nil {
		t.Fatalf("messageBasePath() error = %v", err)
	}
	if err := os.Remove(base + ".emlx"); err != nil {
		t.Fatalf("remove fixture source: %v", err)
	}

	hasAttachment := true
	query, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, HasAttachment: &hasAttachment, Limit: 10,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), query)
	if err != nil || len(page.Messages) != 1 ||
		page.Messages[0].Summary.Subject != "Quarterly Report" ||
		page.Messages[0].Summary.AttachmentCount != 1 ||
		page.Coverage.CatalogProvenMessages != 1 {
		t.Fatalf("attachment=true missing-source page = %#v, error = %v", page, err)
	}
	// Only 101's source is gone; the two catalog-zero candidates (102/103)
	// still scan normally: hybrid coverage, complete because every
	// candidate was either catalog-proven or scanned.
	if page.Coverage.ScannedMessages != 2 || page.Coverage.MissingSources != 0 {
		t.Fatalf("coverage = %#v; the catalog-zero candidates must still be scanned", page.Coverage)
	}
	if !page.Coverage.Complete {
		t.Fatalf("coverage = %#v; catalog-proven plus scanned is complete", page.Coverage)
	}
}

// Catalog-proven candidates decide attachment-only filters without opening
// their source: positive catalog counts prove both filter directions with
// no scan and no byte budget. Catalog-zero candidates stay on the scan
// path (catalog lag), which this fixture makes visible as missing sources.
func TestBodySearchDegradesWhenSourceIsDeletedBeforeScan(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	location, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
	if err != nil {
		t.Fatalf("parseMailboxURL() error = %v", err)
	}
	base, err := store.messageBasePath(location, 101)
	if err != nil {
		t.Fatalf("messageBasePath() error = %v", err)
	}
	if err := os.Remove(base + ".emlx"); err != nil {
		t.Fatalf("remove fixture source: %v", err)
	}
	query, err := mail.PrepareQuery(mail.Query{MailboxRef: inboxRef, Text: "needle", Limit: 10})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), query)
	if err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
	if page.Coverage.MissingSources != 1 || page.Coverage.Complete {
		t.Fatalf("coverage = %#v; deleted source must degrade the page", page.Coverage)
	}
}

func TestBodySearchAbortsOnPermissionDeniedSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks are bypassed for root")
	}
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	location, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
	if err != nil {
		t.Fatalf("parseMailboxURL() error = %v", err)
	}
	base, err := store.messageBasePath(location, 101)
	if err != nil {
		t.Fatalf("messageBasePath() error = %v", err)
	}
	path := base + ".emlx"
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture source: %v", err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("remove fixture source permissions: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(path, info.Mode().Perm())
	})
	query, err := mail.PrepareQuery(mail.Query{MailboxRef: inboxRef, Text: "needle", Limit: 10})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	_, err = store.SearchMessages(context.Background(), query)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("SearchMessages() error = %v, want permission denied", err)
	}
}

func TestSearchSourceErrorClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		err         error
		unavailable bool
	}{
		{name: "typed missing", err: operationError("message_source_missing", "missing"), unavailable: true},
		{name: "typed invalid", err: operationError("invalid_emlx", "invalid"), unavailable: true},
		{name: "wrapped not found", err: fmt.Errorf("open source: %w", fs.ErrNotExist), unavailable: true},
		{name: "permission denied", err: fs.ErrPermission},
		{name: "unexpected", err: errors.New("unexpected source failure")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isUnavailableSearchSourceError(test.err); got != test.unavailable {
				t.Fatalf("isUnavailableSearchSourceError() = %v, want %v", got, test.unavailable)
			}
		})
	}
}

func TestAttachmentOnlySearchSkipsCatalogProvenSources(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	// 101 keeps its catalog attachment row; its source stays on disk.
	// Remove the other sources so a scan attempt would be visible via
	// MissingSources.
	for _, rowID := range []int64{102, 103} {
		location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
		if err != nil {
			t.Fatalf("parseMailboxURL() error = %v", err)
		}
		base, err := store.messageBasePath(location, rowID)
		if err != nil {
			t.Fatalf("messageBasePath() error = %v", err)
		}
		if err := os.Remove(base + ".emlx"); err != nil {
			t.Fatalf("remove fixture source: %v", err)
		}
	}

	hasAttachment := true
	withQuery, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, HasAttachment: &hasAttachment, Limit: 10, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	withPage, err := store.SearchMessages(context.Background(), withQuery)
	if err != nil || len(withPage.Messages) != 1 ||
		withPage.Messages[0].Summary.AttachmentCount != 1 {
		t.Fatalf("attachment=true page = %#v, error = %v", withPage, err)
	}
	if withPage.Coverage.CatalogProvenMessages != 1 {
		t.Fatalf("catalog-proven = %d, want 1 (message 101 decided by catalog)", withPage.Coverage.CatalogProvenMessages)
	}
	// 102/103 have catalog zero, so the scan path still tries them; their
	// sources are removed, making the attempt a missing scan and the
	// coverage honestly incomplete - 101 itself was never opened.
	if withPage.Coverage.MissingSources != 2 || withPage.Coverage.ScannedBytes != 0 {
		t.Fatalf("coverage = %#v; the proven candidate must stay untouched, the zero-count candidates must be attempted",
			withPage.Coverage)
	}
	if withPage.Coverage.Complete {
		t.Fatalf("coverage = %#v; missing zero-count sources keep the search incomplete", withPage.Coverage)
	}

	hasAttachment = false
	withoutQuery, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, HasAttachment: &hasAttachment, Limit: 10,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(without attachment) error = %v", err)
	}
	withoutPage, err := store.SearchMessages(context.Background(), withoutQuery)
	if err != nil {
		t.Fatalf("attachment=false search error = %v", err)
	}
	for _, message := range withoutPage.Messages {
		if message.Summary.AttachmentCount > 0 {
			t.Fatalf("attachment=false matched a catalog-attachment message: %#v", message)
		}
	}
	if withoutPage.Coverage.CatalogProvenMessages != 1 {
		t.Fatalf("catalog-proven = %d, want 1 (negative direction decided by catalog too)", withoutPage.Coverage.CatalogProvenMessages)
	}
}

// Text terms keep the full scan path: a catalog attachment row never
// satisfies a text query.
func TestAttachmentFilterWithTextStillScans(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	hasAttachment := true
	query, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", HasAttachment: &hasAttachment, Limit: 10,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), query)
	if err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
	if page.Coverage.CatalogProvenMessages != 0 || page.Coverage.ScannedMessages == 0 {
		t.Fatalf("coverage = %#v; text terms must keep the scan path", page.Coverage)
	}
}

func TestSearchCursorIsBoundToMailStore(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	first, err := mail.PrepareQuery(mail.Query{MailboxRef: inboxRef, Text: "needle", Limit: 1})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), first)
	if err != nil || page.NextCursor == "" {
		t.Fatalf("SearchMessages() = %+v, error = %v", page, err)
	}
	cursor, err := mail.DecodeSearchCursor(page.NextCursor, first.Fingerprint)
	if err != nil {
		t.Fatalf("DecodeSearchCursor() error = %v", err)
	}
	wrongStore, err := mail.EncodeSearchCursor(
		first.Fingerprint, "different-store", cursor.ReceivedAt, cursor.ReceivedAtNull, cursor.RowID,
	)
	if err != nil {
		t.Fatalf("EncodeSearchCursor() error = %v", err)
	}
	next, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 1, Cursor: wrongStore,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(next) error = %v", err)
	}
	if _, err := store.SearchMessages(context.Background(), next); errorCodeForTest(err) != "invalid_cursor" {
		t.Fatalf("SearchMessages(wrong store) error = %v", err)
	}
}

func TestSearchCursorRejectsIndexMutation(t *testing.T) {
	tests := []struct {
		name      string
		statement string
	}{
		{
			name: "insert",
			statement: `INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color)
				VALUES (104,1004,2004,1,2,2,250,250,1,0,1,0,0,100,4,0,250,0)`,
		},
		{name: "delete", statement: `DELETE FROM messages WHERE ROWID = 103`},
		{name: "move", statement: `UPDATE messages SET mailbox = 2 WHERE ROWID = 102`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, inboxRef := newSearchFixture(t)
			closeTestResource(t, store, "test store")
			first, err := mail.PrepareQuery(mail.Query{MailboxRef: inboxRef, Limit: 1})
			if err != nil {
				t.Fatalf("PrepareQuery(first) error = %v", err)
			}
			page, err := store.SearchMessages(context.Background(), first)
			if err != nil || page.NextCursor == "" {
				t.Fatalf("SearchMessages(first) = %#v, error = %v", page, err)
			}
			mutateSearchIndex(t, store, test.statement)
			next, err := mail.PrepareQuery(mail.Query{
				MailboxRef: inboxRef, Limit: 1, Cursor: page.NextCursor,
			})
			if err != nil {
				t.Fatalf("PrepareQuery(next) error = %v", err)
			}
			if _, err := store.SearchMessages(context.Background(), next); errorCodeForTest(err) != "search_cursor_stale" {
				t.Fatalf("SearchMessages(next) error = %v, want search_cursor_stale", err)
			}
		})
	}
}

func TestSearchCursorReplaysAcrossUnchangedStoreReopen(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	mailRoot := filepath.Dir(store.versionRoot)
	first, err := mail.PrepareQuery(mail.Query{MailboxRef: inboxRef, Limit: 1})
	if err != nil {
		closeTestResourceNow(t, store, "test store")
		t.Fatalf("PrepareQuery(first) error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), first)
	if err != nil || page.NextCursor == "" {
		closeTestResourceNow(t, store, "test store")
		t.Fatalf("SearchMessages(first) = %#v, error = %v", page, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}
	reopened, err := Open(context.Background(), Config{
		MailRoot: mailRoot, ActiveAccountURLs: []string{"imap://" + testAccountID + "/"},
	})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	closeTestResource(t, reopened, "reopened store")
	next, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Limit: 1, Cursor: page.NextCursor,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(next) error = %v", err)
	}
	if _, err := reopened.SearchMessages(context.Background(), next); err != nil {
		t.Fatalf("SearchMessages(next) after reopen error = %v", err)
	}
}

func TestBodySearchSourceReplacementReportsIncompleteCoverage(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	first, err := mail.PrepareQuery(mail.Query{MailboxRef: inboxRef, Text: "needle", Limit: 1})
	if err != nil {
		t.Fatalf("PrepareQuery(first) error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), first)
	if err != nil || page.NextCursor == "" {
		t.Fatalf("SearchMessages(first) = %#v, error = %v", page, err)
	}
	location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
	if err != nil {
		t.Fatalf("parseMailboxURL() error = %v", err)
	}
	base, err := store.messageBasePath(location, 102)
	if err != nil {
		t.Fatalf("messageBasePath() error = %v", err)
	}
	if err := os.WriteFile(base+".emlx", []byte("invalid replacement"), 0o600); err != nil {
		t.Fatalf("replace fixture source: %v", err)
	}
	next, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 1, Cursor: page.NextCursor,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(next) error = %v", err)
	}
	nextPage, err := store.SearchMessages(context.Background(), next)
	if err != nil {
		t.Fatalf("SearchMessages(next) error = %v", err)
	}
	if nextPage.Coverage.Complete || nextPage.Coverage.MissingSources == 0 {
		t.Fatalf("next coverage = %#v, want detectable incomplete replacement", nextPage.Coverage)
	}
}

func mutateSearchIndex(t *testing.T, store *Store, statement string) {
	t.Helper()
	writer := openTestWriter(t, store.databasePath)
	if _, err := writer.Exec(statement); err != nil {
		closeTestResourceNow(t, writer, "search mutation writer")
		t.Fatalf("mutate search index: %v", err)
	}
	if _, err := writer.Exec(
		`UPDATE properties SET value = CAST(value AS INTEGER) + 1 WHERE key = ?`,
		writeTransactionGenerationKey,
	); err != nil {
		closeTestResourceNow(t, writer, "search mutation writer")
		t.Fatalf("advance search index generation: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close search mutation writer: %v", err)
	}
}

// Body-search candidate loading is chunked: resident records stay bounded
// while results, cursors, and coverage stay identical to a monolithic load
// of the same fixture. The baseline reproduces the pre-057 shape inside the
// test: one full-candidate record query plus the original scan loop.
func TestBodySearchChunkedCandidatesMatchMonolithic(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")

	prepared, err := mail.PrepareQuery(mail.Query{MailboxRef: inboxRef, Text: "needle", Limit: 25})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	chunkedPage, err := store.SearchMessages(context.Background(), prepared)
	if err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
	baselinePage, err := monolithicBodySearch(context.Background(), store, prepared)
	if err != nil {
		t.Fatalf("monolithicBodySearch() error = %v", err)
	}

	if len(chunkedPage.Messages) != len(baselinePage.Messages) {
		t.Fatalf("chunked = %d messages, monolithic = %d", len(chunkedPage.Messages), len(baselinePage.Messages))
	}
	for index := range chunkedPage.Messages {
		if chunkedPage.Messages[index] != baselinePage.Messages[index] {
			t.Fatalf("message %d differs: chunked %#v monolithic %#v",
				index, chunkedPage.Messages[index], baselinePage.Messages[index])
		}
	}
	if chunkedPage.Coverage != baselinePage.Coverage {
		t.Fatalf("coverage differs: chunked %#v monolithic %#v",
			chunkedPage.Coverage, baselinePage.Coverage)
	}
	if chunkedPage.NextCursor != baselinePage.NextCursor {
		t.Fatalf("cursor differs: chunked %q monolithic %q",
			chunkedPage.NextCursor, baselinePage.NextCursor)
	}
	if !chunkedPage.Coverage.Complete {
		t.Fatalf("full-corpus chunked scan must stay complete: %#v", chunkedPage.Coverage)
	}
}

// monolithicBodySearch reproduces the pre-057 single-load body-search
// shape: one querySearchRecords call over the full candidate set plus the
// original windowed scan loop, kept as the equality baseline for the
// chunked path.
func monolithicBodySearch(ctx context.Context, store *Store, prepared mail.PreparedQuery) (mail.SearchPage, error) {
	indexRevision, err := store.searchIndexRevision(ctx)
	if err != nil {
		return mail.SearchPage{}, err
	}
	prepared.IndexRevision = indexRevision
	plan, empty, err := store.prepareSearchPlan(ctx, prepared)
	if err != nil || empty {
		return mail.SearchPage{}, err
	}
	maximum := prepared.Query.MaxMessages
	items, err := store.querySearchRecords(ctx, plan, maximum+1)
	if err != nil {
		return mail.SearchPage{}, err
	}
	total, err := store.countSearchCandidates(ctx, plan, maximum)
	if err != nil {
		return mail.SearchPage{}, err
	}
	limitedByCount := len(items) > maximum
	if limitedByCount {
		items = items[:maximum]
	}

	coverage := mail.SearchCoverage{
		Consistency: mail.SearchConsistencyBestEffort, IndexRevision: indexRevision,
		Backend: "emlx_stream", CandidateMessages: total, CandidateMessagesExact: true,
		Complete: !limitedByCount,
	}
	terms := normalizedSearchTerms(prepared.Query.Text)
	results := make([]mail.SearchMessage, 0, prepared.Query.Limit+1)
	resultItems := make([]messageRecord, 0, prepared.Query.Limit+1)
	var reservedBytes int64
	batchSize := min(searchBatchSize, max(searchWorkerCount, prepared.Query.Limit+1))
	for start := 0; start < len(items) && len(results) <= prepared.Query.Limit; {
		matchesBefore := len(results)
		end := min(start+batchSize, len(items))
		scans, budgetLimited, err := store.scanSearchBatch(
			ctx, items[start:end], terms, prepared.Query.HasAttachment,
			prepared.Query.MaxBytes, &reservedBytes,
		)
		if err != nil {
			return mail.SearchPage{}, err
		}
		if budgetLimited {
			coverage.Complete = false
		}
		for index, scan := range scans {
			mergeSearchCoverage(&coverage, scan)
			if !scan.match {
				continue
			}
			summary, err := store.searchSummary(items[start+index], plan.mailbox)
			if err != nil {
				return mail.SearchPage{}, err
			}
			summary.AttachmentCount = scan.attachments
			results = append(results, mail.SearchMessage{Summary: summary, Snippet: scan.snippet})
			resultItems = append(resultItems, items[start+index])
			if len(results) > prepared.Query.Limit {
				break
			}
		}
		start = end
		remaining := prepared.Query.Limit + 1 - len(results)
		if len(results) == matchesBefore {
			batchSize = min(searchBatchSize, batchSize*2)
		} else {
			batchSize = min(searchBatchSize, max(searchWorkerCount, remaining))
		}
		if budgetLimited {
			break
		}
	}
	hasMore := len(results) > prepared.Query.Limit
	if hasMore {
		results = results[:prepared.Query.Limit]
		resultItems = resultItems[:prepared.Query.Limit]
	}
	if coverage.ScannedMessages+coverage.CatalogProvenMessages < coverage.CandidateMessages {
		coverage.Complete = false
	}
	page := mail.SearchPage{Messages: results, Coverage: coverage}
	if hasMore && len(results) > 0 {
		page.NextCursor, err = searchCursorFor(
			resultItems[len(resultItems)-1], prepared.Fingerprint, store.storeUUID, indexRevision,
		)
		return page, err
	}
	return page, nil
}

// NULL date_received rows sort last in SQLite DESC order; the keyset
// cursor must not skip them across a chunk boundary. Oracle: 600 extras
// (chunk 1 = rows 104..615), rows 616..703 NULLed - "needle extra 512"
// (row 616) lives in chunk 2 inside the NULL group. The pre-fix keyset
// skipped all NULL rows, so the chunked path found nothing while the
// monolithic path found the row.
func TestBodySearchChunkHandlesNullDateCandidates(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t, 600)
	closeTestResource(t, store, "test store")
	updateFixtureMessage(t, store, `UPDATE messages SET date_received = NULL WHERE ROWID BETWEEN 616 AND 703`)

	prepared, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle extra 512", Limit: 25,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	chunkedPage, err := store.SearchMessages(context.Background(), prepared)
	if err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
	if len(chunkedPage.Messages) != 1 {
		t.Fatalf("chunked found %d messages, want 1 (the NULL-dated row 616)", len(chunkedPage.Messages))
	}
	baselinePage, err := monolithicBodySearch(context.Background(), store, prepared)
	if err != nil {
		t.Fatalf("monolithicBodySearch() error = %v", err)
	}
	if len(chunkedPage.Messages) != len(baselinePage.Messages) {
		t.Fatalf("chunked = %d messages, monolithic = %d", len(chunkedPage.Messages), len(baselinePage.Messages))
	}
	for index := range chunkedPage.Messages {
		if chunkedPage.Messages[index] != baselinePage.Messages[index] {
			t.Fatalf("message %d differs: chunked %#v monolithic %#v",
				index, chunkedPage.Messages[index], baselinePage.Messages[index])
		}
	}
	if chunkedPage.Coverage != baselinePage.Coverage {
		t.Fatalf("coverage differs: chunked %#v monolithic %#v", chunkedPage.Coverage, baselinePage.Coverage)
	}
	if chunkedPage.NextCursor != baselinePage.NextCursor {
		t.Fatalf("cursor differs: chunked %q monolithic %q", chunkedPage.NextCursor, baselinePage.NextCursor)
	}
}

// The byte-budget stop must survive chunking: with more candidates than
// one chunk (512), an exhausted budget stops the WHOLE scan. Oracle: the
// first candidate alone exceeds MaxBytes=1, so the budget breaks in chunk
// 1; row 616 lives in chunk 2 - if the outer chunk loop kept running, its
// missing source would be attempted and counted.
func TestBodySearchBudgetBreakStopsChunkLoop(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t, 600)
	closeTestResource(t, store, "test store")
	// Remove a chunk-2 source: 600 extras occupy rowIDs 104..703, ordered
	// date_received DESC (newest extra first). Chunk 1 holds the newest
	// 512 (rows 104..615); chunk 2 starts at row 616.
	location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
	if err != nil {
		t.Fatalf("parseMailboxURL() error = %v", err)
	}
	base, err := store.messageBasePath(location, 616)
	if err != nil {
		t.Fatalf("messageBasePath() error = %v", err)
	}
	if err := os.Remove(base + ".emlx"); err != nil {
		t.Fatalf("remove chunk-2 source: %v", err)
	}
	query, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 25,
		MaxBytes: 1, MaxMessages: 100000,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), query)
	if err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
	if page.Coverage.Complete {
		t.Fatalf("budget-limited scan must be incomplete: %#v", page.Coverage)
	}
	if page.Coverage.ScannedBytes > 1 {
		t.Fatalf("scanned bytes %d exceed the budget of 1", page.Coverage.ScannedBytes)
	}
	if page.Coverage.MissingSources != 0 {
		t.Fatalf("missing sources = %d; the outer chunk loop reached chunk 2 after the budget break",
			page.Coverage.MissingSources)
	}
}

func TestBudgetLimitedBodySearchResumesAtLastClassifiedCandidate(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	_, source, err := store.openMessageSource(context.Background(), messageRef)
	if err != nil {
		t.Fatalf("openMessageSource() error = %v", err)
	}
	maxBytes := source.length
	if err := source.Close(); err != nil {
		t.Fatalf("closeMessageSource() error = %v", err)
	}
	first, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 10, MaxBytes: maxBytes,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(first) error = %v", err)
	}
	firstPage, err := store.SearchMessages(context.Background(), first)
	if err != nil {
		t.Fatalf("SearchMessages(first) error = %v", err)
	}
	if len(firstPage.Messages) != 1 || firstPage.Messages[0].Summary.Subject != "Quarterly Report" {
		t.Fatalf("first page = %#v, want only the first match", firstPage)
	}
	if firstPage.Coverage.Complete || firstPage.NextCursor == "" {
		t.Fatalf("first page = %#v, want incomplete continuation", firstPage)
	}
	cursor, err := mail.DecodeSearchCursor(firstPage.NextCursor, first.Fingerprint)
	if err != nil {
		t.Fatalf("DecodeSearchCursor() error = %v", err)
	}
	if cursor.RowID != 101 {
		t.Fatalf("cursor row = %d, want last fully classified row 101", cursor.RowID)
	}
	next, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 10, MaxBytes: maxBytes,
		Cursor: firstPage.NextCursor,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(next) error = %v", err)
	}
	nextPage, err := store.SearchMessages(context.Background(), next)
	if err != nil {
		t.Fatalf("SearchMessages(next) error = %v", err)
	}
	if len(nextPage.Messages) != 1 || nextPage.Messages[0].Summary.Subject != "Status Update" {
		t.Fatalf("next page = %#v, want the previously budget-blocked match", nextPage)
	}
	if !nextPage.Coverage.Complete || nextPage.NextCursor != "" {
		t.Fatalf("next page = %#v, want complete final page", nextPage)
	}
}

func TestCandidateLimitedBodySearchResumesWithoutMatches(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	first, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "does-not-exist", Limit: 10, MaxMessages: 2,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(first) error = %v", err)
	}
	firstPage, err := store.SearchMessages(context.Background(), first)
	if err != nil {
		t.Fatalf("SearchMessages(first) error = %v", err)
	}
	if len(firstPage.Messages) != 0 || firstPage.Coverage.Complete || firstPage.NextCursor == "" {
		t.Fatalf("first page = %#v, want incomplete cursor with zero matches", firstPage)
	}
	cursor, err := mail.DecodeSearchCursor(firstPage.NextCursor, first.Fingerprint)
	if err != nil {
		t.Fatalf("DecodeSearchCursor() error = %v", err)
	}
	if cursor.RowID != 102 {
		t.Fatalf("cursor row = %d, want candidate-limit boundary row 102", cursor.RowID)
	}
	next, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "does-not-exist", Limit: 10, MaxMessages: 2,
		Cursor: firstPage.NextCursor,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(next) error = %v", err)
	}
	nextPage, err := store.SearchMessages(context.Background(), next)
	if err != nil {
		t.Fatalf("SearchMessages(next) error = %v", err)
	}
	if len(nextPage.Messages) != 0 || !nextPage.Coverage.Complete || nextPage.NextCursor != "" {
		t.Fatalf("next page = %#v, want complete empty final page", nextPage)
	}
}

func TestByteLimitedBodySearchRetriesFirstCandidateInclusively(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	first, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 10, MaxBytes: 1,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(first) error = %v", err)
	}
	firstPage, err := store.SearchMessages(context.Background(), first)
	if err != nil {
		t.Fatalf("SearchMessages(first) error = %v", err)
	}
	if len(firstPage.Messages) != 0 || firstPage.Coverage.Complete || firstPage.NextCursor == "" {
		t.Fatalf("first page = %#v, want incomplete inclusive cursor", firstPage)
	}
	cursor, err := mail.DecodeSearchCursor(firstPage.NextCursor, first.Fingerprint)
	if err != nil {
		t.Fatalf("DecodeSearchCursor() error = %v", err)
	}
	if !cursor.Inclusive || cursor.RowID != 101 {
		t.Fatalf("cursor = %+v, want inclusive row 101", cursor)
	}
	next, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 10, MaxBytes: 1,
		Cursor: firstPage.NextCursor,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(next) error = %v", err)
	}
	nextPage, err := store.SearchMessages(context.Background(), next)
	if err != nil {
		t.Fatalf("SearchMessages(next) error = %v", err)
	}
	if len(nextPage.Messages) != 0 || nextPage.NextCursor == "" {
		t.Fatalf("next page = %#v, want the same blocked candidate to remain resumable", nextPage)
	}
	nextCursor, err := mail.DecodeSearchCursor(nextPage.NextCursor, next.Fingerprint)
	if err != nil {
		t.Fatalf("DecodeSearchCursor(next) error = %v", err)
	}
	if !nextCursor.Inclusive || nextCursor.RowID != cursor.RowID {
		t.Fatalf("next cursor = %+v, want unchanged inclusive boundary", nextCursor)
	}
}

// The candidate stream yields strictly-descending (date_received, ROWID)
// tuples: a max-messages bound that cuts mid-corpus keeps the covered
// prefix and reports incomplete, while a one-row lookahead proves an exact
// boundary without running the optional candidate-count query.
func TestBodySearchChunkRespectsMaxMessagesBound(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	// Fixture corpus: 3 needle candidates. Bound at 3 == total: complete.
	exact, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 25, MaxMessages: 3,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), exact)
	if err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
	if page.Coverage.CandidateMessages != 3 || !page.Coverage.CandidateMessagesExact {
		t.Fatalf("candidates = %d, want 3", page.Coverage.CandidateMessages)
	}
	if !page.Coverage.Complete {
		t.Fatalf("exact boundary (total == max) must stay complete: %#v", page.Coverage)
	}

	// Bound at 2 < 3: truncated, incomplete.
	cut, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 25, MaxMessages: 2,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(cut) error = %v", err)
	}
	cutPage, err := store.SearchMessages(context.Background(), cut)
	if err != nil {
		t.Fatalf("SearchMessages(cut) error = %v", err)
	}
	if cutPage.Coverage.CandidateMessages < cutPage.Coverage.ScannedMessages+cutPage.Coverage.CatalogProvenMessages {
		t.Fatalf("coverage counts exceed candidates: %#v", cutPage.Coverage)
	}
	if cutPage.Coverage.CandidateMessagesExact || cutPage.Coverage.Complete {
		t.Fatalf("bound of 2 over 3 candidates must be incomplete: %#v", cutPage.Coverage)
	}
}

func TestBodySearchCandidateCountIsExplicitAndBounded(t *testing.T) {
	tests := []struct {
		name           string
		exactCount     bool
		maxMessages    int
		wantCode       string
		wantQueries    int64
		wantCandidates int
		wantExact      bool
	}{
		{
			name: "default derives a lower bound", maxMessages: 2,
			wantCandidates: 3,
		},
		{
			name: "explicit exact count succeeds within bound", exactCount: true, maxMessages: 3,
			wantQueries: 1, wantCandidates: 3, wantExact: true,
		},
		{
			name: "explicit exact count fails above bound", exactCount: true, maxMessages: 2,
			wantCode: "search_count_limit_exceeded", wantQueries: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, inboxRef := newSearchFixture(t)
			closeTestResource(t, store, "test store")
			prepared, err := mail.PrepareQuery(mail.Query{
				MailboxRef: inboxRef, Text: "needle", Limit: 25,
				MaxMessages: test.maxMessages, ExactCount: test.exactCount,
			})
			if err != nil {
				t.Fatalf("PrepareQuery() error = %v", err)
			}
			page, err := store.SearchMessages(context.Background(), prepared)
			if test.wantCode != "" {
				if errorCodeForTest(err) != test.wantCode {
					t.Fatalf("SearchMessages() error = %v, want %s", err, test.wantCode)
				}
			} else if err != nil {
				t.Fatalf("SearchMessages() error = %v", err)
			}
			if got := store.searchCandidateCountQueries.Load(); got != test.wantQueries {
				t.Fatalf("candidate count queries = %d, want %d", got, test.wantQueries)
			}
			if test.wantCode == "" &&
				(page.Coverage.CandidateMessages != test.wantCandidates ||
					page.Coverage.CandidateMessagesExact != test.wantExact) {
				t.Fatalf("coverage = %+v, want candidates=%d exact=%t", page.Coverage, test.wantCandidates, test.wantExact)
			}
		})
	}
}

func TestSnippetForDoesNotSplitUTF8(t *testing.T) {
	t.Parallel()
	value := strings.Repeat("ä", maximumSnippetRunes+20)
	got := snippetFor(value, "")
	if !utf8.ValidString(got) {
		t.Fatalf("snippetFor() returned invalid UTF-8: %q", got)
	}
	if want := strings.Repeat("ä", maximumSnippetRunes) + "…"; got != want {
		t.Fatalf("snippetFor() = %q, want %q", got, want)
	}
}

func TestSnippetForTracksUnicodeCaseFoldRuneOffsets(t *testing.T) {
	t.Parallel()
	value := strings.Repeat("K", 100) + " target " + strings.Repeat("z", 300)
	collapsed := collapseSearchText(value)
	runes := []rune(collapsed)
	start := 101 - maximumSnippetRunes/3
	want := "…" + string(runes[start:start+maximumSnippetRunes]) + "…"
	if got := snippetFor(value, "TARGET"); got != want {
		t.Fatalf("snippetFor() = %q, want %q", got, want)
	}
}

func TestBuildSearchTextIsDeterministicAndCollapsed(t *testing.T) {
	t.Parallel()
	item := messageRecord{
		Subject: "  Subject\tline ", SenderName: "Alice", SenderAddress: "alice@example.com",
		SummaryText: "summary\ntext",
	}
	document := mimeDocument{
		To:      []mail.Recipient{{Name: "Bob", Address: "bob@example.com"}},
		CC:      []mail.Recipient{{Address: "cc@example.com"}},
		Content: "body\u00a0 text",
		Parts: map[string]mimePart{
			"2": {Name: "zeta.pdf"},
			"1": {Name: "alpha.txt"},
		},
	}
	want := "Subject line Alice alice@example.com summary text Bob bob@example.com " +
		"cc@example.com alpha.txt zeta.pdf body text"
	if got := buildSearchText(item, document); got != want {
		t.Fatalf("buildSearchText() = %q, want %q", got, want)
	}
}

func TestNormalizedSearchTermsDeduplicates(t *testing.T) {
	t.Parallel()
	got := normalizedSearchTerms(" Alpha beta ALPHA beta gamma ")
	want := []string{"alpha", "beta", "gamma"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("normalizedSearchTerms() = %v, want %v", got, want)
	}
}

func TestNormalizedSearchTermsOnePassTokenization(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "ascii lowercase", input: "Hello WORLD", want: []string{"hello", "world"}},
		{name: "unicode lowercase", input: "Café Ünïcode", want: []string{"café", "ünïcode"}},
		{name: "decomposed accent", input: "Cafe\u0301", want: []string{"café"}},
		{name: "german casing", input: "ÜBERTRAGUNG", want: []string{"übertragung"}},
		{name: "greek casing", input: "ΜΗΝΥΜΑ", want: []string{"μηνυμα"}},
		{name: "non-latin text", input: "ПРИВЕТ 世界", want: []string{"привет", "世界"}},
		{name: "tabs and newlines", input: "alpha\tbeta\ngamma", want: []string{"alpha", "beta", "gamma"}},
		{name: "multiple whitespace", input: "  a   b  ", want: []string{"a", "b"}},
		{name: "empty", input: "", want: []string{}},
		{name: "only whitespace", input: "   \t\n  ", want: []string{}},
		{name: "single token", input: "solo", want: []string{"solo"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := normalizedSearchTerms(test.input)
			if fmt.Sprint(got) != fmt.Sprint(test.want) {
				t.Fatalf("normalizedSearchTerms(%q) = %v, want %v", test.input, got, test.want)
			}
		})
	}
}

func TestBuildLoweredSearchTextMatchesCaseInsensitive(t *testing.T) {
	t.Parallel()
	item := messageRecord{
		Subject: "Cafe\u0301 Übertragung Quarterly REPORT", SenderName: "Alice", SenderAddress: "ALICE@EXAMPLE.COM",
	}
	document := mimeDocument{
		Content: "Body TEXT Here",
		Parts: map[string]mimePart{
			"1": {Name: "ÜBER.PDF"},
		},
	}
	lowered := buildLoweredSearchText(item, document)
	if !strings.Contains(lowered, "café übertragung") {
		t.Fatalf("lowered text missing normalized accent: %q", lowered)
	}
	if !strings.Contains(lowered, "übertragung quarterly report") {
		t.Fatalf("lowered text missing Unicode-folded subject: %q", lowered)
	}
	if !strings.Contains(lowered, "quarterly report") {
		t.Fatalf("lowered text missing folded subject: %q", lowered)
	}
	if !strings.Contains(lowered, "alice@example.com") {
		t.Fatalf("lowered text missing folded address: %q", lowered)
	}
	if !strings.Contains(lowered, "body text here") {
		t.Fatalf("lowered text missing folded body: %q", lowered)
	}
	if !strings.Contains(lowered, "über.pdf") {
		t.Fatalf("lowered text missing Unicode-folded attachment name: %q", lowered)
	}
	// Verify it's actually lowered, not original case.
	if strings.Contains(lowered, "REPORT") || strings.Contains(lowered, "ALICE") {
		t.Fatalf("lowered text still contains uppercase: %q", lowered)
	}
}

func TestSearchTextRepresentationsShareAttachmentOrderAndOffsets(t *testing.T) {
	t.Parallel()
	item := messageRecord{}
	document := mimeDocument{
		Parts: map[string]mimePart{
			"2": {Name: "zeta.txt"},
			"1": {Name: "alpha.txt"},
		},
	}
	representations := buildSearchTextRepresentations(item, document)
	if representations.original != "alpha.txt zeta.txt" {
		t.Fatalf("original search text = %q, want sorted attachment names", representations.original)
	}
	if representations.folded != representations.original {
		t.Fatalf("folded search text = %q, want same order as original", representations.folded)
	}
	matched, term := containsAllFoldedSearchTerms(representations.folded, []string{"zeta"})
	if !matched || term != "zeta" {
		t.Fatalf("containsAllFoldedSearchTerms() = %v, %q; want true, zeta", matched, term)
	}
	if got := snippetForSearchText(&representations, term); got != representations.original {
		t.Fatalf("attachment snippet = %q, want %q", got, representations.original)
	}
}

func TestSearchTextRepresentationsUseImplicitIdentityOffsets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		original string
		folded   string
	}{
		{name: "unchanged ascii", original: "alpha target omega", folded: "alpha target omega"},
		{name: "simple lowercase", original: "ÄBC target", folded: "äbc target"},
		{name: "sharp s", original: "Straße target", folded: "straße target"},
		{name: "non-BMP", original: "😀 TARGET", folded: "😀 target"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			representations := newSearchTextRepresentations(test.original)
			if representations.folded != test.folded {
				t.Fatalf("folded text = %q, want %q", representations.folded, test.folded)
			}
			if representations.foldedRuneBoundaries != nil {
				t.Fatalf("identity boundary map = %#v, want nil", representations.foldedRuneBoundaries)
			}
			for _, boundary := range []int{0, 1, utf8.RuneCountInString(test.folded)} {
				if got := originalRuneBoundary(representations.foldedRuneBoundaries, boundary); got != boundary {
					t.Fatalf("originalRuneBoundary(%d) = %d, want identity", boundary, got)
				}
			}
		})
	}
}

func TestSearchTextRepresentationsRetainChangedOffsets(t *testing.T) {
	t.Parallel()
	representations := newSearchTextRepresentations("prefix Cafe\u0301 target suffix")
	if representations.foldedRuneBoundaries != nil {
		t.Fatal("changed normalization eagerly built a boundary map")
	}
	if got := snippetForSearchText(&representations, "TARGET"); !strings.Contains(got, "Cafe\u0301 target") && !strings.Contains(got, "Café target") {
		t.Fatalf("snippetForSearchText() = %q, want target context", got)
	}
	if representations.foldedRuneBoundaries == nil {
		t.Fatal("changed normalization did not build a boundary map for the snippet")
	}
}

func TestSnippetForMapsNormalizedRuneOffsets(t *testing.T) {
	t.Parallel()
	value := strings.Repeat("x", 100) + " Cafe\u0301 target " + strings.Repeat("z", 300)
	got := snippetFor(value, "TARGET")
	if !strings.Contains(got, "Cafe\u0301 target") && !strings.Contains(got, "Café target") {
		t.Fatalf("snippetFor() = %q, want normalized target context", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("snippetFor() returned invalid UTF-8: %q", got)
	}
}

func TestSearchPlanUsesUnicodeFoldFunction(t *testing.T) {
	t.Parallel()
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	prepared, err := mail.PrepareQuery(mail.Query{
		Subject: "ÜBER", Limit: 1,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	plan, empty, err := store.prepareSearchPlan(context.Background(), prepared)
	if err != nil || empty {
		t.Fatalf("prepareSearchPlan() = empty %v, error %v", empty, err)
	}
	if !strings.Contains(plan.fromWhereSQL, searchFoldSQLName) {
		t.Fatalf("search plan = %q, want %s SQL function", plan.fromWhereSQL, searchFoldSQLName)
	}
	var matched bool
	err = store.database.QueryRowContext(
		context.Background(),
		"SELECT "+searchFoldSQL("?")+" LIKE "+searchFoldSQL("?")+" ESCAPE '\\'",
		"%über%", "%ÜBER%",
	).Scan(&matched)
	if err != nil {
		t.Fatalf("Unicode fold SQL query error = %v", err)
	}
	if !matched {
		t.Fatal("Unicode fold SQL query = false, want true")
	}
}

func TestBodySearchUsesUnicodeFoldedMetadataAndContent(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	writeFixtureEMLX(t, store, 101, "imap://"+testAccountID+"/%5BGmail%5D/All", []byte(
		"From: Alice <alice@example.com>\r\n"+
			"Subject: Cafe\u0301\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nÜbertragung\r\n",
	))
	query, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "ÜBER", Limit: 1,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), query)
	if err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Summary.Subject != "Quarterly Report" {
		t.Fatalf("SearchMessages() = %#v, want message 101", page)
	}
}

func TestBodySearchDoesNotOpenCandidatesBeyondSmallResultWindow(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t, 12)
	closeTestResource(t, store, "test store")
	location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
	if err != nil {
		t.Fatalf("parseMailboxURL() error = %v", err)
	}
	unsafeBase, err := store.messageBasePath(location, 106)
	if err != nil {
		t.Fatalf("messageBasePath() error = %v", err)
	}
	if err := os.Remove(unsafeBase + ".emlx"); err != nil {
		t.Fatalf("remove fixture source: %v", err)
	}
	if err := os.Symlink("missing.emlx", unsafeBase+".emlx"); err != nil {
		t.Fatalf("create unsafe fixture source: %v", err)
	}
	query, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 1,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), query)
	if err != nil || len(page.Messages) != 1 || page.NextCursor == "" {
		t.Fatalf("SearchMessages() = %#v, error = %v", page, err)
	}
}

func TestBodySearchStopsLoadingAfterFullPage(t *testing.T) {
	t.Parallel()
	// The newest candidate matches, while the fixture contains 603 rows. A
	// full-page outer-loop regression would load the trailing 91-row chunk.
	store, inboxRef := newSearchFixture(t, 600)
	closeTestResource(t, store, "test store")
	prepared, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 1,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), prepared)
	if err != nil || len(page.Messages) != 1 || page.NextCursor == "" {
		t.Fatalf("SearchMessages() = %#v, error = %v", page, err)
	}
	if got := store.searchCandidateRowsLoaded.Load(); got != int64(candidateChunkSize) {
		t.Fatalf("candidate rows loaded = %d, want first chunk size %d", got, candidateChunkSize)
	}
	if page.Coverage.CandidateMessages != candidateChunkSize || page.Coverage.CandidateMessagesExact || page.Coverage.Complete {
		t.Fatalf("coverage = %#v, want the loaded lower-bound prefix", page.Coverage)
	}
}

func TestBodySearchPaginationPreservesSparseChunkBoundaryMatches(t *testing.T) {
	t.Parallel()
	// Matches at the last row of chunk one and the first row of chunk two
	// exercise the one-row lookahead and the strict cursor anchor together.
	store, inboxRef := newSearchFixture(t, 600)
	closeTestResource(t, store, "test store")
	for _, rowID := range []int64{615, 616, 703} {
		writeFixtureEMLX(t, store, rowID, "imap://"+testAccountID+"/INBOX", sentFixtureSource(rowID, sentMessageFixture{
			Body: "needle sparse",
		}))
	}
	paginated := collectBodySearchRowIDs(t, store, inboxRef, "needle sparse", 1)
	complete := collectBodySearchRowIDs(t, store, inboxRef, "needle sparse", 25)
	want := []string{"615", "616", "703"}
	assertSearchRowIDs(t, paginated, want)
	assertSearchRowIDs(t, complete, want)
}

func TestBodySearchDensePaginationPreservesOrder(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t, 600)
	closeTestResource(t, store, "test store")
	got := collectBodySearchRowIDs(t, store, inboxRef, "needle", 25)
	want := make([]string, 0, 602)
	for rowID := int64(104); rowID <= 703; rowID++ {
		want = append(want, fmt.Sprintf("%d", rowID))
	}
	want = append(want, "101", "102")
	assertSearchRowIDs(t, got, want)
}

func collectBodySearchRowIDs(t testing.TB, store *Store, mailboxRef, text string, limit int) []string {
	t.Helper()
	var rowIDs []string
	cursor := ""
	for pageNumber := 0; pageNumber < 1000; pageNumber++ {
		prepared, err := mail.PrepareQuery(mail.Query{
			MailboxRef: mailboxRef, Text: text, Limit: limit, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("PrepareQuery(page %d) error = %v", pageNumber, err)
		}
		page, err := store.SearchMessages(context.Background(), prepared)
		if err != nil {
			t.Fatalf("SearchMessages(page %d) error = %v", pageNumber, err)
		}
		for _, message := range page.Messages {
			ref, err := mailref.DecodeMessage(message.Summary.Ref)
			if err != nil {
				t.Fatalf("DecodeMessage(page %d) error = %v", pageNumber, err)
			}
			rowIDs = append(rowIDs, ref.LibraryID)
		}
		if page.NextCursor == "" {
			if !page.Coverage.Complete {
				t.Fatalf("final page %d coverage = %#v, want complete", pageNumber, page.Coverage)
			}
			return rowIDs
		}
		if page.NextCursor == cursor {
			t.Fatalf("page %d repeated cursor %q", pageNumber, cursor)
		}
		cursor = page.NextCursor
	}
	t.Fatal("body search pagination did not terminate")
	return nil
}

func assertSearchRowIDs(t testing.TB, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("search rows = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("search row %d = %q, want %q (all rows: %v)", index, got[index], want[index], got)
		}
	}
}

func newSearchFixture(t testing.TB, extraMessages ...int) (*Store, string) {
	t.Helper()
	extraCount := 0
	if len(extraMessages) > 1 || len(extraMessages) == 1 && extraMessages[0] < 0 {
		t.Fatal("newSearchFixture() accepts at most one non-negative extra-message count")
	}
	if len(extraMessages) == 1 {
		extraCount = extraMessages[0]
	}
	root := t.TempDir()
	mailRoot := filepath.Join(root, "Mail")
	mailData := filepath.Join(mailRoot, "V10", "MailData")
	if err := os.MkdirAll(mailData, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	databasePath := filepath.Join(mailData, envelopeIndexName)
	writer := openTestWriter(t, databasePath)
	createTestSchema(t, writer, "")
	statements := []string{
		`INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source) VALUES (1,'imap://` + testAccountID + `/INBOX',3,0,0,1)`,
		`INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source) VALUES (2,'imap://` + testAccountID + `/%5BGmail%5D/All',1,0,0,1)`,
		`INSERT INTO addresses(ROWID,address,comment) VALUES (1,'alice@example.com','Alice')`,
		`INSERT INTO addresses(ROWID,address,comment) VALUES (2,'christopher@example.com','Christopher')`,
		`INSERT INTO subjects(ROWID,subject) VALUES (1,'Quarterly Report'),(2,'Status Update'),(3,'Noise')`,
		`INSERT INTO summaries(ROWID,summary) VALUES (1,'report summary'),(2,'status summary'),(3,'noise summary')`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (101,1001,2001,1,1,1,300,300,2,0,0,1,0,100,1,0,300,0)`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (102,1002,2002,1,2,2,200,200,1,0,1,0,0,100,2,0,200,0)`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (103,1003,2003,1,3,3,100,100,1,0,1,0,0,100,3,0,100,0)`,
		`INSERT INTO labels(message_id,mailbox_id) VALUES (101,1)`,
		`INSERT INTO recipients(message,address,type,position) VALUES (101,2,0,0),(102,2,0,0),(103,2,0,0)`,
		`INSERT INTO attachments(message,attachment_id,name) VALUES (101,'2','invoice.pdf')`,
	}
	for _, statement := range statements {
		if _, err := writer.Exec(statement); err != nil {
			closeTestResourceNow(t, writer, "fixture database")
			t.Fatalf("execute fixture statement: %v", err)
		}
	}
	for index := range extraCount {
		rowID := int64(104 + index)
		received := int64(1000 - index)
		_, err := writer.Exec(
			`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			rowID, 1000+rowID, 2000+rowID, 1, 2, 2, received, received, 1,
			0, 1, 0, 0, 100, rowID, 0, received, 0,
		)
		if err != nil {
			closeTestResourceNow(t, writer, "fixture database")
			t.Fatalf("insert extra fixture message: %v", err)
		}
		if _, err := writer.Exec(
			`INSERT INTO recipients(message,address,type,position) VALUES (?,2,0,0)`, rowID,
		); err != nil {
			closeTestResourceNow(t, writer, "fixture database")
			t.Fatalf("insert extra fixture recipient: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close fixture database: %v", err)
	}
	store, err := Open(context.Background(), Config{
		MailRoot:          mailRoot,
		ActiveAccountURLs: []string{"imap://" + testAccountID + "/"},
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	locations := map[int64]string{101: "imap://" + testAccountID + "/%5BGmail%5D/All", 102: "imap://" + testAccountID + "/INBOX", 103: "imap://" + testAccountID + "/INBOX"}
	bodies := map[int64]string{
		101: "needle alpha " + strings.Repeat("x", 512),
		102: "needle beta",
		103: "ordinary content",
	}
	for index := range extraCount {
		rowID := int64(104 + index)
		locations[rowID] = "imap://" + testAccountID + "/INBOX"
		bodies[rowID] = fmt.Sprintf("needle extra %d", index)
	}
	for rowID, mailboxURL := range locations {
		location, err := parseMailboxURL(mailboxURL)
		if err != nil {
			closeTestResourceNow(t, store, "test store")
			t.Fatalf("parseMailboxURL() error = %v", err)
		}
		base, err := store.messageBasePath(location, rowID)
		if err != nil {
			closeTestResourceNow(t, store, "test store")
			t.Fatalf("messageBasePath() error = %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
			closeTestResourceNow(t, store, "test store")
			t.Fatalf("MkdirAll(message) error = %v", err)
		}
		source := []byte(fmt.Sprintf(
			"From: Alice <alice@example.com>\r\nTo: Christopher <christopher@example.com>\r\nSubject: Test\r\nMessage-ID: <%d@example.com>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n",
			rowID, bodies[rowID],
		))
		framed := append([]byte(fmt.Sprintf("%-10d\n", len(source))), source...)
		framed = append(framed, validPlistTrailer()...)
		if err := os.WriteFile(base+".emlx", framed, 0o600); err != nil {
			closeTestResourceNow(t, store, "test store")
			t.Fatalf("WriteFile(message) error = %v", err)
		}
	}
	inboxRef, err := mailref.EncodeMailbox(testAccountID, []string{"INBOX"})
	if err != nil {
		closeTestResourceNow(t, store, "test store")
		t.Fatalf("EncodeMailbox() error = %v", err)
	}
	return store, inboxRef
}
