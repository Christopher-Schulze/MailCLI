package mailstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

const (
	searchWorkerCount   = 2
	searchBatchSize     = 64
	maximumSnippetRunes = 240
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
	plan, empty, err := s.prepareSearchPlan(ctx, prepared)
	if err != nil {
		return mail.SearchPage{}, err
	}
	sourceScan := requiresSourceScan(prepared.Query)
	if empty {
		return emptySearchPage(sourceScan), nil
	}
	if !sourceScan {
		return s.searchMetadata(ctx, prepared, plan)
	}
	return s.searchBodies(ctx, prepared, plan)
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
	records, err := s.loadMailboxRecords(ctx)
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
		*where = append(*where, "(sender.address LIKE ? ESCAPE '\\' OR sender.comment LIKE ? ESCAPE '\\')")
		value := containsLike(query.Sender)
		*arguments = append(*arguments, value, value)
	}
	if query.Recipient != "" {
		*where = append(*where, `EXISTS (
			SELECT 1 FROM recipients recipient
			JOIN addresses recipient_address ON recipient_address.ROWID = recipient.address
			WHERE recipient.message = m.ROWID
			AND (recipient_address.address LIKE ? ESCAPE '\' OR recipient_address.comment LIKE ? ESCAPE '\')
		)`)
		value := containsLike(query.Recipient)
		*arguments = append(*arguments, value, value)
	}
	if query.Subject != "" {
		*where = append(*where, "subject.subject LIKE ? ESCAPE '\\'")
		*arguments = append(*arguments, containsLike(query.Subject))
	}
	appendScalarFilters(where, arguments, prepared)
}

func appendScalarFilters(where *[]string, arguments *[]any, prepared mail.PreparedQuery) {
	query := prepared.Query
	if prepared.AfterUnix != 0 {
		*where = append(*where, "m.date_received >= ?")
		*arguments = append(*arguments, prepared.AfterUnix)
	}
	if prepared.BeforeUnix != 0 {
		*where = append(*where, "m.date_received < ?")
		*arguments = append(*arguments, prepared.BeforeUnix)
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
		*where = append(*where, "(m.date_received < ? OR (m.date_received = ? AND m.ROWID < ?))")
		*arguments = append(*arguments, prepared.Cursor.ReceivedAt, prepared.Cursor.ReceivedAt, prepared.Cursor.RowID)
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
	items, total, err := s.querySearchRecords(ctx, plan, prepared.Query.Limit+1)
	if err != nil {
		return mail.SearchPage{}, err
	}
	hasMore := len(items) > prepared.Query.Limit
	if hasMore {
		items = items[:prepared.Query.Limit]
	}
	page, err := s.mapSearchRecords(items, prepared, plan.mailbox)
	if err != nil {
		return mail.SearchPage{}, err
	}
	page.Coverage = mail.SearchCoverage{
		Backend: "envelope_sql", CandidateMessages: total, Complete: true,
	}
	if hasMore && len(items) > 0 {
		page.NextCursor, err = searchCursorFor(items[len(items)-1], prepared.Fingerprint, s.storeUUID)
	}
	return page, err
}

func (s *Store) searchBodies(
	ctx context.Context,
	prepared mail.PreparedQuery,
	plan searchPlan,
) (mail.SearchPage, error) {
	maximum := prepared.Query.MaxMessages
	items, total, err := s.querySearchRecords(ctx, plan, maximum+1)
	if err != nil {
		return mail.SearchPage{}, err
	}
	limitedByCount := len(items) > maximum
	if limitedByCount {
		items = items[:maximum]
	}
	return s.scanSearchRecords(ctx, prepared, plan.mailbox, items, total, limitedByCount)
}

func (s *Store) querySearchRecords(
	ctx context.Context,
	plan searchPlan,
	limit int,
) (result []messageRecord, total int, resultErr error) {
	query := `SELECT
		m.ROWID, COALESCE(m.message_id, 0), COALESCE(m.global_message_id, 0),
		m.mailbox, mb.url,
		subject.subject, sender.address, sender.comment,
		COALESCE(summary.summary, ''), COALESCE(m.date_sent, 0),
		COALESCE(m.date_received, 0), m.read, m.flagged, m.deleted,
		EXISTS (SELECT 1 FROM server_messages sm WHERE sm.message = m.ROWID AND sm.junk_level > 0),
		m.size, (SELECT count(*) FROM attachments attachment WHERE attachment.message = m.ROWID),
		count(*) OVER ()
	` + plan.fromWhereSQL + " ORDER BY m.date_received DESC, m.ROWID DESC LIMIT ?"
	query = plan.prefixSQL + query
	arguments := append(append([]any(nil), plan.arguments...), limit)
	rows, err := s.database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, 0, fmt.Errorf("query Envelope Index search candidates: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "search candidate rows")
	var items []messageRecord
	for rows.Next() {
		var item messageRecord
		if err := rows.Scan(
			&item.RowID, &item.StoreMessageID, &item.StoreGlobalID, &item.StoreMailboxID,
			&item.PhysicalURL,
			&item.Subject, &item.SenderAddress, &item.SenderName, &item.SummaryText,
			&item.DateSent, &item.DateReceived, &item.Read, &item.Flagged, &item.Deleted,
			&item.Junk, &item.Size, &item.AttachmentCount, &total,
		); err != nil {
			return nil, 0, fmt.Errorf("scan Envelope Index search candidate: %w", err)
		}
		if items == nil {
			items = make([]messageRecord, 0, min(limit, total))
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate Envelope Index search candidates: %w", err)
	}
	return items, total, nil
}

func (s *Store) mapSearchRecords(
	items []messageRecord,
	prepared mail.PreparedQuery,
	mailbox *mailref.Mailbox,
) (mail.SearchPage, error) {
	messages := make([]mail.SearchMessage, 0, len(items))
	for _, item := range items {
		summary, err := s.searchSummary(item, mailbox)
		if err != nil {
			return mail.SearchPage{}, err
		}
		messages = append(messages, mail.SearchMessage{
			Summary: summary, Snippet: snippetFor(item.SummaryText, prepared.Query.Subject),
		})
	}
	return mail.SearchPage{Messages: messages}, nil
}

func (s *Store) searchSummary(item messageRecord, mailbox *mailref.Mailbox) (mail.MessageSummary, error) {
	location, err := parseMailboxURL(item.PhysicalURL)
	if err != nil {
		return mail.MessageSummary{}, operationError("unsupported_mail_store_schema", err.Error())
	}
	accountID := location.AccountID
	path := location.VisiblePath
	if mailbox != nil {
		accountID = mailbox.AccountID
		path = mailbox.Path
	}
	mailboxRef, err := mailref.EncodeMailbox(accountID, path)
	if err != nil {
		return mail.MessageSummary{}, err
	}
	return mapMessageSummary(item, mailboxRef, accountID, path, s.storeUUID)
}

func (s *Store) scanSearchRecords(
	ctx context.Context,
	prepared mail.PreparedQuery,
	mailbox *mailref.Mailbox,
	items []messageRecord,
	total int,
	limitedByCount bool,
) (mail.SearchPage, error) {
	coverage := mail.SearchCoverage{Backend: "emlx_stream", CandidateMessages: total, Complete: !limitedByCount}
	terms := normalizedSearchTerms(prepared.Query.Text)
	results := make([]mail.SearchMessage, 0, prepared.Query.Limit+1)
	resultItems := make([]messageRecord, 0, prepared.Query.Limit+1)
	var reservedBytes int64
	batchSize := min(searchBatchSize, max(searchWorkerCount, prepared.Query.Limit+1))
	for start := 0; start < len(items) && len(results) <= prepared.Query.Limit; {
		matchesBefore := len(results)
		end := min(start+batchSize, len(items))
		scans, budgetLimited, err := s.scanSearchBatch(
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
			summary, err := s.searchSummary(items[start+index], mailbox)
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
	if coverage.ScannedMessages < coverage.CandidateMessages {
		coverage.Complete = false
	}
	page := mail.SearchPage{Messages: results, Coverage: coverage}
	if hasMore && len(results) > 0 {
		var err error
		page.NextCursor, err = searchCursorFor(
			resultItems[len(resultItems)-1], prepared.Fingerprint, s.storeUUID,
		)
		return page, err
	}
	return page, nil
}

func (s *Store) scanSearchBatch(
	ctx context.Context,
	items []messageRecord,
	terms []string,
	hasAttachment *bool,
	maximumBytes int64,
	reservedBytes *int64,
) ([]candidateScan, bool, error) {
	results := make([]candidateScan, len(items))
	group, groupContext := errgroup.WithContext(ctx)
	jobs := make(chan searchJob)
	for worker := 0; worker < min(searchWorkerCount, len(items)); worker++ {
		group.Go(func() error {
			for job := range jobs {
				if err := groupContext.Err(); err != nil {
					return err
				}
				scan, err := scanCandidate(groupContext, job.item, terms, hasAttachment, job.source)
				if err != nil {
					return err
				}
				results[job.index] = scan
			}
			return nil
		})
	}
	budgetLimited := false
	group.Go(func() error {
		defer close(jobs)
		var err error
		budgetLimited, err = s.dispatchSearchJobs(
			groupContext, items, terms, hasAttachment, maximumBytes, reservedBytes, results, jobs,
		)
		return err
	})
	if err := group.Wait(); err != nil {
		return nil, false, err
	}
	return results, budgetLimited, nil
}

func (s *Store) dispatchSearchJobs(
	ctx context.Context,
	items []messageRecord,
	terms []string,
	hasAttachment *bool,
	maximumBytes int64,
	reservedBytes *int64,
	results []candidateScan,
	jobs chan<- searchJob,
) (bool, error) {
	for index, item := range items {
		source, unavailable, err := s.openSearchCandidate(item)
		if err != nil {
			return false, err
		}
		if source == nil {
			unavailable.attachments = item.AttachmentCount
			unavailable.match = len(terms) == 0 && hasAttachment != nil &&
				*hasAttachment && item.AttachmentCount > 0
			results[index] = unavailable
			continue
		}
		if source.length < 0 || *reservedBytes > maximumBytes-source.length {
			if err := source.Close(); err != nil {
				return false, fmt.Errorf("close byte-limited search source: %w", err)
			}
			return true, nil
		}
		select {
		case jobs <- searchJob{index: index, item: item, source: source}:
			// Only count reserved bytes after the job was successfully sent.
			*reservedBytes += source.length
		case <-ctx.Done():
			resultErr := ctx.Err()
			joinCloseError(&resultErr, source, "cancelled search source")
			return false, resultErr
		}
	}
	return false, nil
}

func (s *Store) openSearchCandidate(item messageRecord) (*emlxSource, candidateScan, error) {
	location, err := parseMailboxURL(item.PhysicalURL)
	if err != nil {
		return nil, candidateScan{}, operationError("unsupported_mail_store_schema", err.Error())
	}
	resolved := resolvedMessage{Record: item, PhysicalLocation: location}
	source, err := s.openResolvedSource(resolved)
	if err != nil {
		var typed *Error
		if errors.As(err, &typed) && (typed.Code == "message_source_missing" || typed.Code == "invalid_emlx") {
			return nil, candidateScan{missing: true}, nil
		}
		return nil, candidateScan{}, err
	}
	return source, candidateScan{}, nil
}

func scanCandidate(
	ctx context.Context,
	item messageRecord,
	terms []string,
	hasAttachment *bool,
	source *emlxSource,
) (result candidateScan, resultErr error) {
	defer joinCloseError(&resultErr, source, "search source")
	if err := ctx.Err(); err != nil {
		return candidateScan{}, err
	}
	document, err := parseMIMEDocument(source.Reader(), source.partial, false, true)
	if err != nil {
		knownAttachmentMatch := len(terms) == 0 && hasAttachment != nil &&
			*hasAttachment && item.AttachmentCount > 0
		return candidateScan{
			match: knownAttachmentMatch, attachments: item.AttachmentCount,
			bytes: source.length, partial: source.partial, full: !source.partial, contentWhole: false,
		}, nil
	}
	match := true
	attachmentCount := max(item.AttachmentCount, len(document.Parts))
	if hasAttachment != nil {
		if *hasAttachment {
			match = attachmentCount > 0
		} else {
			match = document.Complete && attachmentCount == 0
		}
	}
	snippet := ""
	if match {
		folded := buildLoweredSearchText(item, document)
		firstTerm := ""
		if len(terms) > 0 {
			match, firstTerm = containsAllFoldedSearchTerms(folded, terms)
		}
		if match {
			// Build original-case text only for the snippet to preserve
			// readable case in search results.
			haystack := buildSearchText(item, document)
			snippet = snippetForFolded(haystack, folded, firstTerm)
		}
	}
	return candidateScan{
		match: match, snippet: snippet, bytes: source.length,
		attachments: attachmentCount,
		partial:     source.partial, full: !source.partial, contentWhole: document.Complete,
	}, nil
}

func mergeSearchCoverage(coverage *mail.SearchCoverage, scan candidateScan) {
	if scan.full || scan.partial || scan.missing {
		coverage.ScannedMessages++
	}
	coverage.ScannedBytes += scan.bytes
	if scan.full {
		coverage.FullSources++
	}
	if scan.partial {
		coverage.PartialSources++
	}
	if scan.missing {
		coverage.MissingSources++
	}
	if !scan.contentWhole || scan.missing {
		coverage.Complete = false
	}
}

func normalizedSearchTerms(value string) []string {
	// Fold+tokenize in one pass, avoiding strings.ToLower allocation.
	var terms []string
	var current []byte
	flush := func() {
		if len(current) > 0 {
			term := string(current)
			current = current[:0]
			duplicate := false
			for _, existing := range terms {
				if term == existing {
					duplicate = true
					break
				}
			}
			if !duplicate {
				terms = append(terms, term)
			}
		}
	}
	for i := 0; i < len(value); {
		c := value[i]
		if c < utf8.RuneSelf {
			if c <= ' ' || isASCIIWhitespace(c) {
				flush()
				i++
				continue
			}
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			current = append(current, c)
			i++
			continue
		}
		// Multi-byte: lowercase via unicode
		r, size := utf8.DecodeRuneInString(value[i:])
		if unicode.IsSpace(r) {
			flush()
		} else {
			r = unicode.ToLower(r)
			current = append(current, string(r)...)
		}
		i += size
	}
	flush()
	return terms
}

func containsAllFoldedSearchTerms(folded string, terms []string) (bool, string) {
	first := ""
	for _, term := range terms {
		if !strings.Contains(folded, term) {
			return false, ""
		}
		if first == "" {
			first = term
		}
	}
	return len(terms) > 0, first
}

func snippetFor(value string, term string) string {
	value = collapseSearchText(value)
	return snippetForFolded(value, strings.ToLower(value), strings.ToLower(term))
}

func snippetForFolded(value string, folded string, term string) string {
	if value == "" {
		return ""
	}
	runeCount := utf8.RuneCountInString(value)
	start := 0
	if term != "" {
		byteIndex := strings.Index(folded, term)
		if byteIndex > 0 {
			start = utf8.RuneCountInString(folded[:byteIndex]) - maximumSnippetRunes/3
			if start < 0 {
				start = 0
			}
		}
	}
	end := min(start+maximumSnippetRunes, runeCount)
	prefix := ""
	suffix := ""
	if start > 0 {
		prefix = "…"
	}
	if end < runeCount {
		suffix = "…"
	}
	startByte, endByte := runeByteRangeFast(value, start, end)
	return prefix + value[startByte:endByte] + suffix
}

// runeByteRangeFast converts rune indices to byte indices in value.
func runeByteRangeFast(value string, startRune int, endRune int) (int, int) {
	index := 0
	startByte := 0
	for count := 0; count < endRune && index < len(value); count++ {
		if count == startRune {
			startByte = index
		}
		_, size := utf8.DecodeRuneInString(value[index:])
		index += size
	}
	if startRune == endRune {
		startByte = index
	}
	return startByte, index
}

func searchCursorFor(item messageRecord, fingerprint string, storeUUID string) (string, error) {
	return mail.EncodeSearchCursor(fingerprint, storeUUID, item.DateReceived, item.RowID)
}

func buildSearchText(item messageRecord, document mimeDocument) string {
	var builder collapsedSearchTextBuilder
	builder.Grow(searchTextCapacity(item, document))
	for _, value := range []string{
		item.Subject, item.SenderName, item.SenderAddress, item.SummaryText,
	} {
		builder.Add(value)
	}
	for _, recipients := range [][]mail.Recipient{document.To, document.CC, document.BCC} {
		for _, recipient := range recipients {
			builder.Add(recipient.Name)
			builder.Add(recipient.Address)
		}
	}
	names := make([]string, 0, len(document.Parts))
	for _, part := range document.Parts {
		names = append(names, part.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		builder.Add(name)
	}
	builder.Add(document.Content)
	return builder.String()
}

// buildLoweredSearchText builds the same collapsed search text as
// buildSearchText but with ASCII bytes lowercased, eliminating the
// strings.ToLower allocation that the previous approach required.
// Part names are not sorted (not needed for matching, only for display).
func buildLoweredSearchText(item messageRecord, document mimeDocument) string {
	var builder collapsedSearchTextBuilder
	builder.Grow(searchTextCapacity(item, document))
	for _, value := range []string{
		item.Subject, item.SenderName, item.SenderAddress, item.SummaryText,
	} {
		builder.AddLowered(value)
	}
	for _, recipients := range [][]mail.Recipient{document.To, document.CC, document.BCC} {
		for _, recipient := range recipients {
			builder.AddLowered(recipient.Name)
			builder.AddLowered(recipient.Address)
		}
	}
	for _, part := range document.Parts {
		builder.AddLowered(part.Name)
	}
	builder.AddLowered(document.Content)
	return builder.String()
}

func searchTextCapacity(item messageRecord, document mimeDocument) int {
	size := len(item.Subject) + len(item.SenderName) + len(item.SenderAddress) +
		len(item.SummaryText) + len(document.Content) + 5 + len(document.Parts)
	for _, recipients := range [][]mail.Recipient{document.To, document.CC, document.BCC} {
		size += 2 * len(recipients)
		for _, recipient := range recipients {
			size += len(recipient.Name) + len(recipient.Address)
		}
	}
	for _, part := range document.Parts {
		size += len(part.Name)
	}
	return size
}

type collapsedSearchTextBuilder struct {
	output       strings.Builder
	pendingSpace bool
}

func (b *collapsedSearchTextBuilder) Grow(size int) {
	b.output.Grow(size)
}

func (b *collapsedSearchTextBuilder) Add(value string) {
	if value != "" && b.output.Len() > 0 {
		b.pendingSpace = true
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		// ASCII whitespace (0x00-0x20) and UTF-8 non-breaking space (0xC2 0xA0)
		if c <= ' ' || (c == 0xC2 && i+1 < len(value) && value[i+1] == 0xA0) {
			if c == 0xC2 {
				i++ // skip the 0xA0 byte too
			}
			b.pendingSpace = b.output.Len() > 0
			continue
		}
		if b.pendingSpace {
			b.output.WriteByte(' ')
			b.pendingSpace = false
		}
		b.output.WriteByte(c)
	}
}

// AddLowered adds value with ASCII bytes lowercased (A-Z → a-z) and whitespace
// collapsed, avoiding a separate strings.ToLower allocation on the full haystack.
func (b *collapsedSearchTextBuilder) AddLowered(value string) {
	if value != "" && b.output.Len() > 0 {
		b.pendingSpace = true
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c <= ' ' || (c == 0xC2 && i+1 < len(value) && value[i+1] == 0xA0) {
			if c == 0xC2 {
				i++
			}
			b.pendingSpace = b.output.Len() > 0
			continue
		}
		if b.pendingSpace {
			b.output.WriteByte(' ')
			b.pendingSpace = false
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.output.WriteByte(c)
	}
}

func (b *collapsedSearchTextBuilder) String() string {
	return b.output.String()
}

func containsLike(value string) string {
	return "%" + escapeLike(value) + "%"
}

func escapeLike(value string) string {
	var builder strings.Builder
	builder.Grow(len(value) + strings.Count(value, "\\") + strings.Count(value, "%") + strings.Count(value, "_"))
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			builder.WriteString("\\\\")
		case '%':
			builder.WriteString("\\%")
		case '_':
			builder.WriteString("\\_")
		default:
			builder.WriteByte(value[i])
		}
	}
	return builder.String()
}

func emptySearchPage(sourceScan bool) mail.SearchPage {
	backend := "envelope_sql"
	if sourceScan {
		backend = "emlx_stream"
	}
	return mail.SearchPage{
		Messages: []mail.SearchMessage{},
		Coverage: mail.SearchCoverage{Backend: backend, Complete: true},
	}
}
