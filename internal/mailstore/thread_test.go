package mailstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

const inactiveThreadAccountID = "FFFFFFFF-EEEE-4DDD-8CCC-BBBBBBBBBBBB"
const threadStoreUUID = "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"

// newThreadFixture builds a store where conversation 42 spans INBOX, Sent,
// and one mailbox of an account that is not active. The ungrouped message
// has a NULL conversation_id like Mail leaves on unthreaded mail.
func newThreadFixture(t *testing.T) (*Store, string) {
	return newThreadFixtureWithOptions(t, 0, threadStoreUUID)
}

func newThreadFixtureWithOptions(t *testing.T, visiblePrefix int, storeUUID string) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	mailRoot := filepath.Join(root, "Mail")
	mailData := filepath.Join(mailRoot, "V10", "MailData")
	if err := os.MkdirAll(mailData, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	databasePath := filepath.Join(mailData, envelopeIndexName)
	writer := openTestWriter(t, databasePath)
	createTestSchema(t, writer, "")
	if _, err := writer.Exec(`UPDATE properties SET value = ? WHERE key = 'UUID'`, storeUUID); err != nil {
		closeTestResourceNow(t, writer, "fixture database")
		t.Fatalf("set fixture store UUID: %v", err)
	}
	statements := []string{
		`INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source) VALUES (1,'imap://` + testAccountID + `/INBOX',2,0,0,1)`,
		`INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source) VALUES (2,'imap://` + testAccountID + `/Sent',1,0,0,1)`,
		`INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source) VALUES (3,'imap://` + inactiveThreadAccountID + `/INBOX',1,0,0,1)`,
		`INSERT INTO addresses(ROWID,address,comment) VALUES (1,'alice@example.com','Alice'),(2,'christopher@example.com','Christopher')`,
		`INSERT INTO subjects(ROWID,subject) VALUES (1,'Thread start'),(2,'Thread reply'),(3,'Solo'),(4,'Inactive member'),(5,'Deleted member'),(6,'Thread prefix')`,
		`INSERT INTO summaries(ROWID,summary) VALUES (1,'first'),(2,'reply'),(3,'solo'),(4,'inactive'),(5,'deleted'),(6,'prefix')`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (11,1101,2101,1,1,1,100,100,1,0,0,0,0,100,42,0,100,0)`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (12,1102,2102,2,2,2,200,200,2,0,1,0,0,100,42,0,200,0)`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (13,1103,2103,1,3,3,300,300,1,0,0,0,0,100,NULL,0,300,0)`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (14,1104,2104,1,5,5,50,50,1,0,0,0,1,100,42,0,50,0)`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (15,1105,2105,1,4,4,150,40,3,0,0,0,0,100,42,0,40,0)`,
		`INSERT INTO recipients(message,address,type,position) VALUES (11,2,0,0),(12,1,0,0),(13,2,0,0)`,
	}
	for index := range visiblePrefix {
		rowID := 100 + index
		messageID := 1200 + index
		globalMessageID := 2200 + index
		receivedAt := 60 + index
		statements = append(statements, fmt.Sprintf(
			`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (%d,%d,%d,1,6,6,%d,%d,1,0,0,0,0,100,42,0,%d,0)`,
			rowID, messageID, globalMessageID, receivedAt, receivedAt, receivedAt,
		))
	}
	for _, statement := range statements {
		if _, err := writer.Exec(statement); err != nil {
			closeTestResourceNow(t, writer, "fixture database")
			t.Fatalf("execute fixture statement: %v", err)
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
	inboxRef, err := mailref.EncodeMailbox(testAccountID, []string{"INBOX"})
	if err != nil {
		closeTestResourceNow(t, store, "thread store")
		t.Fatalf("EncodeMailbox() error = %v", err)
	}
	return store, inboxRef
}

func threadSeedRef(t *testing.T, store *Store, mailboxRef string, subject string) string {
	t.Helper()
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: mailboxRef, Limit: mail.MaximumPageLimit,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	for _, message := range page.Messages {
		if message.Subject == subject {
			return message.Ref
		}
	}
	t.Fatalf("no fixture message with subject %q", subject)
	return ""
}

func TestMessageThreadSpansMailboxesChronologically(t *testing.T) {
	store, inboxRef := newThreadFixture(t)
	closeTestResource(t, store, "thread store")
	seedRef := threadSeedRef(t, store, inboxRef, "Thread start")

	thread, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: seedRef, Limit: mail.DefaultPageLimit,
	})
	if err != nil {
		t.Fatalf("MessageThread() error = %v", err)
	}
	if thread.Ref != seedRef || thread.ConversationID != 42 || thread.Truncated {
		t.Fatalf("thread header = %+v", thread)
	}
	// Deleted member 14 and inactive-account member 15 are excluded; the two
	// visible members arrive in chronological order.
	if len(thread.Messages) != 2 {
		t.Fatalf("members = %+v", thread.Messages)
	}
	if thread.Messages[0].Subject != "Thread start" || thread.Messages[1].Subject != "Thread reply" {
		t.Fatalf("member order = %q, %q", thread.Messages[0].Subject, thread.Messages[1].Subject)
	}
	for _, member := range thread.Messages {
		if member.ConversationID != 42 {
			t.Fatalf("member projection = %+v", member)
		}
		decoded, err := mailref.DecodeMessage(member.Ref)
		if err != nil || !decoded.IsStoreBound() {
			t.Fatalf("member ref %q = %v, %v", member.Ref, decoded, err)
		}
	}
	sentRef, err := mailref.EncodeMailbox(testAccountID, []string{"Sent"})
	if err != nil {
		t.Fatalf("EncodeMailbox() error = %v", err)
	}
	if thread.Messages[0].MailboxRef != inboxRef || thread.Messages[1].MailboxRef != sentRef {
		t.Fatalf("member mailbox refs = %q, %q", thread.Messages[0].MailboxRef, thread.Messages[1].MailboxRef)
	}
}

func TestMessageThreadTruncatesAtLimit(t *testing.T) {
	store, inboxRef := newThreadFixture(t)
	closeTestResource(t, store, "thread store")
	seedRef := threadSeedRef(t, store, inboxRef, "Thread start")

	thread, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: seedRef, Limit: 1,
	})
	if err != nil {
		t.Fatalf("MessageThread() error = %v", err)
	}
	if !thread.Truncated || len(thread.Messages) != 1 || thread.Messages[0].Subject != "Thread start" {
		t.Fatalf("truncated thread = %+v", thread)
	}
}

func TestMessageThreadPagesAllVisibleMembersFromSeedInBothDirections(t *testing.T) {
	store, inboxRef := newThreadFixtureWithOptions(t, 30, threadStoreUUID)
	closeTestResource(t, store, "thread store")
	seedRef := threadSeedRef(t, store, inboxRef, "Thread start")
	first, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: seedRef, Limit: mail.MaximumPageLimit,
	})
	if err != nil {
		t.Fatalf("MessageThread(first) error = %v", err)
	}
	if first.Ref != seedRef || first.ConversationID != 42 || !first.Truncated || first.PrevCursor == "" || first.NextCursor != "" ||
		len(first.Messages) != mail.MaximumPageLimit {
		t.Fatalf("first page ref=%q conversation=%d truncated=%t cursor=%t members=%d",
			first.Ref, first.ConversationID, first.Truncated, first.NextCursor != "", len(first.Messages))
	}
	if first.Messages[23].Ref != seedRef {
		t.Fatalf("initial page does not contain its seed: %+v", first.Messages)
	}
	second, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: seedRef, Limit: mail.MaximumPageLimit, Cursor: first.PrevCursor,
	})
	if err != nil {
		t.Fatalf("MessageThread(second) error = %v", err)
	}
	if second.Ref != seedRef || second.ConversationID != first.ConversationID || !second.Truncated ||
		second.PrevCursor != "" || second.NextCursor == "" || len(second.Messages) != 7 {
		t.Fatalf("second page = %+v", second)
	}
	all := append(append([]mail.MessageSummary(nil), second.Messages...), first.Messages...)
	if len(all) != 32 {
		t.Fatalf("visible member count = %d, want 32", len(all))
	}
	seen := make(map[string]struct{}, len(all))
	seedCount := 0
	previousDate := ""
	for _, member := range all {
		if _, exists := seen[member.Ref]; exists {
			t.Fatalf("member %q appeared more than once", member.Ref)
		}
		seen[member.Ref] = struct{}{}
		if member.Ref == seedRef {
			seedCount++
		}
		if member.Subject == "Inactive member" || member.Subject == "Deleted member" {
			t.Fatalf("invisible member consumed a page slot: %+v", member)
		}
		if member.DateReceived < previousDate {
			t.Fatalf("member order regressed from %q to %q", previousDate, member.DateReceived)
		}
		previousDate = member.DateReceived
	}
	if seedCount != 1 {
		t.Fatalf("seed occurrence = %d", seedCount)
	}
	roundTrip, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: seedRef, Limit: mail.MaximumPageLimit, Cursor: second.NextCursor,
	})
	if err != nil || !reflect.DeepEqual(roundTrip.Messages, first.Messages) {
		t.Fatalf("forward round trip = %+v, error = %v", roundTrip, err)
	}
}

func TestMessageThreadCursorToleratesIndexMutation(t *testing.T) {
	store, inboxRef := newThreadFixture(t)
	closeTestResource(t, store, "thread store")
	seedRef := threadSeedRef(t, store, inboxRef, "Thread start")
	first, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: seedRef, Limit: 1,
	})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("MessageThread(first) = %+v, error = %v", first, err)
	}
	mutateSearchIndex(t, store, `UPDATE messages SET read = 1 WHERE ROWID = 12`)
	next, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: seedRef, Limit: 1, Cursor: first.NextCursor,
	})
	if err != nil || len(next.Messages) != 1 || next.Messages[0].Subject != "Thread reply" || !next.Messages[0].Read {
		t.Fatalf("MessageThread(after unrelated write) = %+v, error = %v", next, err)
	}
}

func TestMessageThreadCursorRejectsCrossStoreReuse(t *testing.T) {
	firstStore, firstMailboxRef := newThreadFixtureWithOptions(t, 1, threadStoreUUID)
	closeTestResource(t, firstStore, "first thread store")
	secondStore, secondMailboxRef := newThreadFixtureWithOptions(t, 1, "11111111-2222-4333-8444-555555555555")
	closeTestResource(t, secondStore, "second thread store")
	firstSeed := threadSeedRef(t, firstStore, firstMailboxRef, "Thread start")
	first, err := firstStore.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: firstSeed, Limit: 1,
	})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("MessageThread(first store) = %+v, error = %v", first, err)
	}
	secondSeed := threadSeedRef(t, secondStore, secondMailboxRef, "Thread start")
	_, err = secondStore.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: secondSeed, Limit: 1, Cursor: first.NextCursor,
	})
	if errorCodeForTest(err) != "invalid_cursor" {
		t.Fatalf("MessageThread(cross-store cursor) error = %v, want invalid_cursor", err)
	}
}

func TestMessageThreadUngroupedSeedReturnsSingleMember(t *testing.T) {
	store, inboxRef := newThreadFixture(t)
	closeTestResource(t, store, "thread store")
	seedRef := threadSeedRef(t, store, inboxRef, "Solo")

	thread, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: seedRef, Limit: mail.DefaultPageLimit,
	})
	if err != nil {
		t.Fatalf("MessageThread() error = %v", err)
	}
	if thread.ConversationID != 0 || len(thread.Messages) != 1 || thread.Messages[0].Ref != seedRef {
		t.Fatalf("ungrouped thread = %+v", thread)
	}
}

func TestMessageThreadValidatesRequest(t *testing.T) {
	store, inboxRef := newThreadFixture(t)
	closeTestResource(t, store, "thread store")
	seedRef := threadSeedRef(t, store, inboxRef, "Thread start")

	for _, limit := range []int{0, mail.MaximumPageLimit + 1} {
		if _, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
			Ref: seedRef, Limit: limit,
		}); errorCodeForTest(err) != "invalid_argument" {
			t.Fatalf("limit %d error = %v, want invalid_argument", limit, err)
		}
	}
	if _, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: "stale-ref", Limit: mail.DefaultPageLimit,
	}); err == nil {
		t.Fatal("stale ref must fail before reading members")
	}
}

func TestClientMessageThreadFailsClosedWithoutStore(t *testing.T) {
	client := &Client{}
	if _, err := client.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: "ref", Limit: mail.DefaultPageLimit,
	}); err == nil {
		t.Fatal("MessageThread without a store must fail closed")
	}
}

func threadMemberIDs(t *testing.T, page mail.MessageThread) []int64 {
	t.Helper()
	ids := make([]int64, len(page.Messages))
	for index, member := range page.Messages {
		ref, err := mailref.DecodeMessage(member.Ref)
		if err != nil {
			t.Fatal(err)
		}
		ids[index], err = strconv.ParseInt(ref.LibraryID, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func TestMessageThreadSeedPagesAndNullBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		seed  int64
		limit int
		want  []int64
		older bool
		newer bool
	}{
		{name: "first", seed: 100, limit: 3, want: []int64{100, 101, 102}, newer: true},
		{name: "middle", seed: 103, limit: 3, want: []int64{102, 103, 104}, older: true, newer: true},
		{name: "last", seed: 12, limit: 3, want: []int64{104, 11, 12}, older: true},
		{name: "NULL middle", seed: 101, limit: 3, want: []int64{100, 101, 102}, newer: true},
		{name: "one", seed: 102, limit: 1, want: []int64{102}, older: true, newer: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, _ := newThreadFixtureWithOptions(t, 5, threadStoreUUID)
			closeTestResource(t, store, "thread store")
			mutateSearchIndex(t, store, `UPDATE messages SET date_received = CASE WHEN ROWID IN (100,101,102) THEN NULL ELSE 0 END WHERE ROWID BETWEEN 100 AND 104`)
			items, err := store.conversationRecords(context.Background(), 42, nil, 10)
			if err != nil {
				t.Fatal(err)
			}
			seedRef := ""
			for _, item := range items {
				if item.RowID != test.seed {
					continue
				}
				summary, err := store.threadSummary(item)
				if err != nil {
					t.Fatal(err)
				}
				seedRef = summary.Ref
			}
			if seedRef == "" {
				t.Fatal("seed missing")
			}
			page, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{Ref: seedRef, Limit: test.limit})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(threadMemberIDs(t, page), test.want) || (page.PrevCursor != "") != test.older ||
				(page.NextCursor != "") != test.newer || page.Truncated != (test.older || test.newer) {
				t.Fatalf("seed page = %+v, IDs=%v", page, threadMemberIDs(t, page))
			}
			// Reach the oldest page, then follow every forward boundary. This
			// crosses multiple NULL pages and the NULL-to-real-zero boundary.
			for steps := 0; page.PrevCursor != ""; steps++ {
				if steps > 7 {
					t.Fatal("previous cursor did not advance")
				}
				page, err = store.MessageThread(context.Background(), mail.MessageThreadRequest{Ref: seedRef, Limit: test.limit, Cursor: page.PrevCursor})
				if err != nil {
					t.Fatal(err)
				}
			}
			var all []int64
			for steps := 0; ; steps++ {
				if steps > 7 {
					t.Fatal("next cursor did not advance")
				}
				all = append(all, threadMemberIDs(t, page)...)
				if page.NextCursor == "" {
					break
				}
				page, err = store.MessageThread(context.Background(), mail.MessageThreadRequest{Ref: seedRef, Limit: test.limit, Cursor: page.NextCursor})
				if err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(all, []int64{100, 101, 102, 103, 104, 11, 12}) {
				t.Fatalf("complete traversal = %v", all)
			}
			// A complete reverse traversal proves the non-NULL-to-NULL branch.
			all = threadMemberIDs(t, page)
			for steps := 0; page.PrevCursor != ""; steps++ {
				if steps > 7 {
					t.Fatal("reverse cursor did not advance")
				}
				page, err = store.MessageThread(context.Background(), mail.MessageThreadRequest{Ref: seedRef, Limit: test.limit, Cursor: page.PrevCursor})
				if err != nil {
					t.Fatal(err)
				}
				all = append(threadMemberIDs(t, page), all...)
			}
			if !reflect.DeepEqual(all, []int64{100, 101, 102, 103, 104, 11, 12}) {
				t.Fatalf("reverse traversal = %v", all)
			}
		})
	}
}

func TestMessageThreadInsertionsFollowKeysetVisibility(t *testing.T) {
	store, inboxRef := newThreadFixture(t)
	closeTestResource(t, store, "thread store")
	seed := threadSeedRef(t, store, inboxRef, "Thread start")
	first, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{Ref: seed, Limit: 1})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	mutateSearchIndex(t, store, `INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color)
		VALUES (16,1116,2116,1,6,6,90,90,1,0,0,0,0,100,42,0,90,0),(17,1117,2117,1,6,6,150,150,1,0,0,0,0,100,42,0,150,0)`)
	mutateSearchIndex(t, store, `UPDATE messages SET read = 1 WHERE ROWID = 13`)
	next, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{Ref: seed, Limit: 2, Cursor: first.NextCursor})
	if err != nil || !reflect.DeepEqual(threadMemberIDs(t, next), []int64{17, 12}) {
		t.Fatalf("continued page=%+v err=%v", next, err)
	}
	restarted, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{Ref: seed, Limit: 4})
	if err != nil || !reflect.DeepEqual(threadMemberIDs(t, restarted), []int64{16, 11, 17, 12}) {
		t.Fatalf("restarted page=%+v err=%v", restarted, err)
	}
}

func TestMessageThreadLegacyCursorAndInvalidIdentities(t *testing.T) {
	store, inboxRef := newThreadFixture(t)
	closeTestResource(t, store, "thread store")
	seed := threadSeedRef(t, store, inboxRef, "Thread start")
	payload := mailref.CompactPayload{Fingerprint: threadCursorFingerprint(seed, 42), StoreUUID: store.storeUUID,
		IndexRevision: "historical-generation", DateReceived: 100, RowID: 11}
	legacy, err := mailref.EncodeCompactTokenPayload(threadCursorPrefix, &payload, legacyThreadCursorVersion)
	if err != nil {
		t.Fatal(err)
	}
	mutateSearchIndex(t, store, `UPDATE messages SET read = 1 WHERE ROWID = 13`)
	page, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{Ref: seed, Limit: 1, Cursor: legacy})
	if err != nil || !reflect.DeepEqual(threadMemberIDs(t, page), []int64{12}) {
		t.Fatalf("legacy page=%+v err=%v", page, err)
	}
	tests := []struct {
		name        string
		version     byte
		flags       uint8
		rowID       int64
		fingerprint string
	}{
		{name: "legacy direction flag", version: 1, flags: 2, rowID: 11, fingerprint: payload.Fingerprint},
		{name: "unknown flag", version: 2, flags: 4, rowID: 11, fingerprint: payload.Fingerprint},
		{name: "unknown version", version: 3, rowID: 11, fingerprint: payload.Fingerprint},
		{name: "missing row", version: 2, rowID: 0, fingerprint: payload.Fingerprint},
		{name: "other conversation", version: 2, rowID: 11, fingerprint: threadCursorFingerprint(seed, 43)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := payload
			invalid.Flags, invalid.RowID, invalid.Fingerprint = test.flags, test.rowID, test.fingerprint
			cursor, err := mailref.EncodeCompactTokenPayload(threadCursorPrefix, &invalid, test.version)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.MessageThread(context.Background(), mail.MessageThreadRequest{Ref: seed, Limit: 1, Cursor: cursor})
			if errorCodeForTest(err) != "invalid_cursor" {
				t.Fatalf("invalid identity accepted: %v", err)
			}
		})
	}
}

func TestMessageThreadVisibilityMatchesNormalizedRoots(t *testing.T) {
	store, inboxRef := newThreadFixture(t)
	closeTestResource(t, store, "thread store")
	seed := threadSeedRef(t, store, inboxRef, "Thread start")
	writer := openTestWriter(t, store.databasePath)
	closeTestResource(t, writer, "visibility writer")
	property := func(bits uint64) bool {
		scheme, account, path := "imap", testAccountID, "Folder"
		if bits&1 != 0 {
			scheme = "IMaP"
		}
		if bits&2 != 0 {
			account = strings.ToLower(account)
		}
		if bits&4 != 0 {
			path = "%46older%20Name"
		}
		if bits&8 != 0 {
			account = inactiveThreadAccountID
		}
		if bits&16 != 0 {
			scheme = "local"
		}
		value := scheme + "://" + account + "/" + path
		location, err := parseMailboxURL(value)
		if err != nil {
			t.Errorf("generated URL %q: %v", value, err)
			return false
		}
		_, wantVisible := store.activeAccountKeys[location.rootKey()]
		clause, arguments := store.threadAccountSQL()
		var visible bool
		if err := store.database.QueryRowContext(context.Background(), "SELECT "+clause+" FROM (SELECT ? AS url) mb", append(arguments, value)...).Scan(&visible); err != nil {
			t.Errorf("SQL visibility %q: %v", value, err)
			return false
		}
		if visible != wantVisible {
			return false
		}
		if _, err := writer.Exec(`UPDATE mailboxes SET url = ? WHERE ROWID = 2`, value); err != nil {
			t.Errorf("set generated URL: %v", err)
			return false
		}
		page, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{Ref: seed, Limit: 2})
		want := []int64{11}
		if wantVisible {
			want = append(want, 12)
		}
		return err == nil && reflect.DeepEqual(threadMemberIDs(t, page), want) && !page.Truncated
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 128}); err != nil {
		t.Fatal(err)
	}
	invalidPaths := []string{"Folder/", "%2F", "..", "%00", "%0A", "Folder?query", "Folder#fragment", "Folder\nChild"}
	for _, path := range invalidPaths {
		t.Run(path, func(t *testing.T) {
			value := "imap://" + testAccountID + "/" + path
			if _, err := writer.Exec(`UPDATE mailboxes SET url = ? WHERE ROWID = 2`, value); err != nil {
				t.Fatal(err)
			}
			_, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{Ref: seed, Limit: 2})
			if errorCodeForTest(err) != "unsupported_mail_store_schema" {
				t.Fatalf("unsafe URL %q error=%v", value, err)
			}
		})
	}
	_, err := store.threadSummary(messageRecord{PhysicalURL: "imap://" + inactiveThreadAccountID + "/INBOX"})
	if errorCodeForTest(err) != "unsupported_mail_store_schema" {
		t.Fatalf("visibility assertion error=%v", err)
	}
}
