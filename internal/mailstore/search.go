package mailstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

const (
	searchWorkerCount   = 2
	searchBatchSize     = 64
	maximumSnippetRunes = 240
	searchFoldSQLName   = "mailcli_search_fold"
)

type searchPlan struct {
	prefixSQL    string
	fromWhereSQL string
	arguments    []any
	mailbox      *mailref.Mailbox
}

type candidateScan struct {
	match        bool
	snippet      string
	bytes        int64
	attachments  int
	full         bool
	partial      bool
	missing      bool
	contentWhole bool
	// catalogProven marks an attachment-only match decided by the
	// AttachmentCount catalog without opening the source.
	catalogProven bool
	processed     bool
	budgetLimited bool
	requiredBytes int64
}

type searchJob struct {
	index  int
	item   messageRecord
	source *emlxSource
}

func (s *Store) SearchMessages(ctx context.Context, prepared mail.PreparedQuery) (mail.SearchPage, error) {
	if prepared.Cursor != nil && prepared.Cursor.StoreUUID != s.storeUUID {
		return mail.SearchPage{}, operationError("invalid_cursor", "search cursor belongs to a different Mail store")
	}
	indexRevision, err := s.searchIndexRevision(ctx)
	if err != nil {
		return mail.SearchPage{}, err
	}
	if prepared.Cursor != nil && prepared.Cursor.IndexRevision != indexRevision {
		return mail.SearchPage{}, operationError(
			"search_cursor_stale",
			"Mail's Envelope Index changed after the previous search page; restart the search without a cursor",
		)
	}
	prepared.IndexRevision = indexRevision
	plan, empty, err := s.prepareSearchPlan(ctx, prepared)
	if err != nil {
		return mail.SearchPage{}, err
	}
	sourceScan := requiresSourceScan(prepared.Query)
	var page mail.SearchPage
	if empty {
		page = emptySearchPage(sourceScan)
	} else if !sourceScan {
		page, err = s.searchMetadata(ctx, prepared, plan)
	} else {
		page, err = s.searchBodies(ctx, prepared, plan)
	}
	if err != nil {
		var budgetError *searchBudgetTooSmallError
		if errors.As(err, &budgetError) {
			currentRevision, revisionErr := s.searchIndexRevision(ctx)
			if revisionErr != nil {
				return mail.SearchPage{}, revisionErr
			}
			if currentRevision != indexRevision {
				return mail.SearchPage{}, operationError(
					"search_index_changed",
					"Mail's Envelope Index changed during the search; retry the page",
				)
			}
		}
		return mail.SearchPage{}, err
	}
	currentRevision, err := s.searchIndexRevision(ctx)
	if err != nil {
		return mail.SearchPage{}, err
	}
	if currentRevision != indexRevision {
		return mail.SearchPage{}, operationError(
			"search_index_changed",
			"Mail's Envelope Index changed during the search; retry the page",
		)
	}
	page.Coverage.Consistency = mail.SearchConsistencyBestEffort
	page.Coverage.IndexRevision = indexRevision
	return page, nil
}

func requiresSourceScan(query mail.Query) bool {
	return strings.TrimSpace(query.Text) != "" || query.HasAttachment != nil
}

func (s *Store) prepareSearchPlan(
	ctx context.Context,
	prepared mail.PreparedQuery,
) (searchPlan, bool, error) {
	query := prepared.Query
	accountID, mailbox, mailboxRowID, empty, err := s.resolveSearchScope(ctx, query)
	if err != nil || empty {
		return searchPlan{}, empty, err
	}
	cte := ""
	from := `
		FROM messages m
		JOIN mailboxes mb ON mb.ROWID = m.mailbox
		JOIN subjects subject ON subject.ROWID = m.subject
		JOIN addresses sender ON sender.ROWID = m.sender
		LEFT JOIN summaries summary ON summary.ROWID = m.summary
	`
	arguments := make([]any, 0, 16)
	where := []string{"m.deleted = 0"}
	if mailbox != nil {
		cte = `WITH membership(id) AS (
			SELECT ROWID FROM messages WHERE mailbox = ?
			UNION
			SELECT message_id FROM labels WHERE mailbox_id = ?
		) `
		from = strings.Replace(from, "FROM messages m", "FROM membership membership JOIN messages m ON m.ROWID = membership.id", 1)
		arguments = append(arguments, mailboxRowID, mailboxRowID)
	} else if accountID != "" {
		where = append(where, "UPPER(mb.url) LIKE ? ESCAPE '\\'")
		arguments = append(arguments, "%://"+escapeLike(strings.ToUpper(accountID))+"/%")
	} else {
		clause, values := s.activeAccountSQL()
		where = append(where, clause)
		arguments = append(arguments, values...)
	}
	appendMetadataFilters(&where, &arguments, prepared)
	return searchPlan{
		prefixSQL: cte, fromWhereSQL: from + " WHERE " + strings.Join(where, " AND "),
		arguments: arguments, mailbox: mailbox,
	}, false, nil
}

func (s *Store) resolveSearchScope(
	ctx context.Context,
	query mail.Query,
) (string, *mailref.Mailbox, int64, bool, error) {
	accountID, err := s.requestedAccountID(query.AccountRef)
	if err != nil {
		return "", nil, 0, false, err
	}
	if query.MailboxRef == "" {
		return accountID, nil, 0, false, nil
	}
	mailbox, err := mailref.DecodeMailbox(query.MailboxRef)
	if err != nil {
		return "", nil, 0, false, operationError("invalid_reference", fmt.Sprintf("invalid mailbox ref: %v", err))
	}
	if !s.activeAccountID(mailbox.AccountID) {
		return "", nil, 0, false, operationError("stale_reference", "mailbox account is not active")
	}
	if accountID != "" && !strings.EqualFold(accountID, mailbox.AccountID) {
		return "", nil, 0, false, operationError("invalid_argument", "account and mailbox refs select different accounts")
	}
	records, err := s.mailboxRecords(ctx)
	if err != nil {
		return "", nil, 0, false, err
	}
	record, found := findMailboxRecord(records, mailbox.AccountID, mailbox.Path)
	if found {
		return mailbox.AccountID, &mailbox, record.RowID, false, nil
	}
	mailboxes, err := s.ListMailboxes(ctx, mail.ListMailboxesRequest{})
	if err != nil {
		return "", nil, 0, false, err
	}
	for _, candidate := range mailboxes {
		if candidate.Ref == query.MailboxRef && !candidate.LocalMessagesAvailable {
			return mailbox.AccountID, &mailbox, 0, true, nil
		}
	}
	return "", nil, 0, false, operationError("not_found", "mailbox is not present in the local Mail store")
}

func appendMetadataFilters(where *[]string, arguments *[]any, prepared mail.PreparedQuery) {
	query := prepared.Query
	if query.Sender != "" {
		*where = append(*where,
			"("+searchFoldSQL("sender.address")+" LIKE "+searchFoldSQL("?")+" ESCAPE '\\' OR "+
				searchFoldSQL("sender.comment")+" LIKE "+searchFoldSQL("?")+" ESCAPE '\\')",
		)
		value := containsLike(query.Sender)
		*arguments = append(*arguments, value, value)
	}
	if query.Recipient != "" {
		*where = append(*where, `EXISTS (
			SELECT 1 FROM recipients recipient
			JOIN addresses recipient_address ON recipient_address.ROWID = recipient.address
			WHERE recipient.message = m.ROWID
			AND (`+searchFoldSQL("recipient_address.address")+` LIKE `+searchFoldSQL("?")+` ESCAPE '\' OR `+
			searchFoldSQL("recipient_address.comment")+` LIKE `+searchFoldSQL("?")+` ESCAPE '\')
		)`)
		value := containsLike(query.Recipient)
		*arguments = append(*arguments, value, value)
	}
	if query.Subject != "" {
		*where = append(*where, searchFoldSQL("subject.subject")+" LIKE "+searchFoldSQL("?")+" ESCAPE '\\'")
		*arguments = append(*arguments, containsLike(query.Subject))
	}
	appendScalarFilters(where, arguments, prepared)
}

func searchFoldSQL(value string) string {
	return searchFoldSQLName + "(" + value + ")"
}

func appendScalarFilters(where *[]string, arguments *[]any, prepared mail.PreparedQuery) {
	query := prepared.Query
	if prepared.AfterUnix != nil {
		*where = append(*where, "m.date_received >= ?")
		*arguments = append(*arguments, *prepared.AfterUnix)
	}
	if prepared.BeforeUnix != nil {
		*where = append(*where, "m.date_received < ?")
		*arguments = append(*arguments, *prepared.BeforeUnix)
	}
	if query.Read != nil {
		*where = append(*where, "m.read = ?")
		*arguments = append(*arguments, *query.Read)
	}
	if query.Flagged != nil {
		*where = append(*where, "m.flagged = ?")
		*arguments = append(*arguments, *query.Flagged)
	}
	if prepared.Cursor != nil {
		if prepared.Cursor.ReceivedAtNull {
			rowOperator := "<"
			if prepared.Cursor.Inclusive {
				rowOperator = "<="
			}
			*where = append(*where, "m.date_received IS NULL AND m.ROWID "+rowOperator+" ?")
			*arguments = append(*arguments, prepared.Cursor.RowID)
		} else {
			rowOperator := "<"
			if prepared.Cursor.Inclusive {
				rowOperator = "<="
			}
			*where = append(*where,
				"((m.date_received < ? OR (m.date_received = ? AND m.ROWID "+rowOperator+" ?)) OR m.date_received IS NULL)",
			)
			*arguments = append(*arguments, prepared.Cursor.ReceivedAt, prepared.Cursor.ReceivedAt, prepared.Cursor.RowID)
		}
	}
}

func (s *Store) activeAccountSQL() (string, []any) {
	keys := make([]string, 0, len(s.activeAccountKeys))
	for key := range s.activeAccountKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	clauses := make([]string, 0, len(keys))
	values := make([]any, 0, len(keys))
	for _, key := range keys {
		clauses = append(clauses, "UPPER(mb.url) LIKE ? ESCAPE '\\'")
		values = append(values, escapeLike(strings.ToUpper(key))+"/%")
	}
	if len(clauses) == 0 {
		return "0", nil
	}
	return "(" + strings.Join(clauses, " OR ") + ")", values
}

func (s *Store) searchMetadata(
	ctx context.Context,
	prepared mail.PreparedQuery,
	plan searchPlan,
) (mail.SearchPage, error) {
	items, err := s.querySearchRecords(ctx, plan, prepared.Query.Limit+1)
	if err != nil {
		return mail.SearchPage{}, err
	}
	s.searchMetadataRowsLoaded.Add(int64(len(items)))
	hasMore := len(items) > prepared.Query.Limit
	candidateMessages := len(items)
	candidateMessagesExact := !hasMore
	if prepared.Query.ExactCount {
		candidateMessages, err = s.countSearchCandidates(ctx, plan, prepared.Query.MaxMessages)
		if err != nil {
			return mail.SearchPage{}, err
		}
		candidateMessagesExact = true
	}
	if hasMore {
		items = items[:prepared.Query.Limit]
	}
	page, err := s.mapSearchRecords(items, prepared, plan.mailbox)
	if err != nil {
		return mail.SearchPage{}, err
	}
	page.Coverage = mail.SearchCoverage{
		Backend: "envelope_sql", CandidateMessages: candidateMessages,
		CandidateMessagesExact: candidateMessagesExact, Complete: true,
	}
	if hasMore && len(items) > 0 {
		page.NextCursor, err = searchCursorFor(
			items[len(items)-1], prepared.Fingerprint, s.storeUUID, prepared.IndexRevision,
		)
	}
	return page, err
}
