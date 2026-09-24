package mailstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"golang.org/x/sync/errgroup"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

type searchBudgetTooSmallError struct {
	requiredBytes int64
	maximumBytes  int64
}

func (e *searchBudgetTooSmallError) Error() string {
	return fmt.Sprintf(
		"search candidate requires %d RFC bytes, above --max-bytes %d; restart the same search without --cursor and set --max-bytes to at least %d",
		e.requiredBytes, e.maximumBytes, e.requiredBytes,
	)
}

func (e *searchBudgetTooSmallError) ErrorCode() string {
	return "search_budget_too_small"
}

func (e *searchBudgetTooSmallError) RequiredBytes() int64 {
	return e.requiredBytes
}

func (s *Store) searchBodies(
	ctx context.Context,
	prepared mail.PreparedQuery,
	plan searchPlan,
) (mail.SearchPage, error) {
	total := 0
	totalExact := false
	if prepared.Query.ExactCount {
		var err error
		total, err = s.countSearchCandidates(ctx, plan, prepared.Query.MaxMessages)
		if err != nil {
			return mail.SearchPage{}, err
		}
		totalExact = true
	}
	maximum := prepared.Query.MaxMessages
	stream := s.newSearchCandidateStream(ctx, plan)
	return s.scanSearchRecordsChunked(ctx, prepared, plan.mailbox, stream, total, totalExact, maximum)
}

// scanSearchRecordsChunked pulls body-search candidates in chunks and
// keeps the previous per-window scan, budget, and coverage semantics.
func (s *Store) scanSearchRecordsChunked(
	ctx context.Context,
	prepared mail.PreparedQuery,
	mailbox *mailref.Mailbox,
	stream *searchCandidateStream,
	total int,
	totalExact bool,
	maximum int,
) (mail.SearchPage, error) {
	coverage := mail.SearchCoverage{
		Backend: "emlx_stream", CandidateMessages: total, CandidateMessagesExact: totalExact, Complete: true,
	}
	terms := normalizedSearchTerms(prepared.Query.Text)
	results := make([]mail.SearchMessage, 0, prepared.Query.Limit+1)
	var reservedBytes int64
	batchSize := min(searchBatchSize, max(searchWorkerCount, prepared.Query.Limit+1))
	loaded := 0
	// progressCount tracks candidates fully classified by the scan. A
	// byte-blocked candidate stays at an inclusive continuation boundary; if
	// no candidate was classified, the caller receives a terminal budget error.
	progressCount := 0
	var lastScanned *messageRecord
	var budgetCandidate *messageRecord
	var budgetRequiredBytes int64
chunkLoop:
	// A loaded chunk can contain an unscanned tail after the page fills. Keep
	// that tail out of later queries: lastScanned remains the resumable cursor.
	for loaded < maximum && len(results) < prepared.Query.Limit {
		chunkWant := min(candidateChunkSize, maximum-loaded)
		items, err := stream.next(chunkWant)
		if err != nil {
			return mail.SearchPage{}, err
		}
		if len(items) == 0 {
			break
		}
		chunkStart := loaded
		loaded += len(items)
		for start := 0; start < len(items) && len(results) < prepared.Query.Limit; {
			matchesBefore := len(results)
			remaining := prepared.Query.Limit - len(results)
			end := min(start+min(batchSize, remaining), len(items))
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
				if scan.processed && chunkStart+start+index+1 > progressCount {
					progressCount = chunkStart + start + index + 1
					lastScanned = &items[start+index]
				}
				if scan.budgetLimited {
					budgetCandidate = &items[start+index]
					budgetRequiredBytes = scan.requiredBytes
				}
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
			}
			start = end
			if len(results) == matchesBefore {
				batchSize = min(searchBatchSize, batchSize*2)
			} else {
				batchSize = min(searchBatchSize, max(searchWorkerCount, prepared.Query.Limit-len(results)))
			}
			if budgetLimited {
				break chunkLoop
			}
		}
	}
	if !totalExact {
		observed := loaded
		if loaded == progressCount && !stream.done {
			lookahead, err := stream.next(1)
			if err != nil {
				return mail.SearchPage{}, err
			}
			observed += len(lookahead)
		}
		coverage.CandidateMessages = observed
		coverage.CandidateMessagesExact = stream.done
	}
	coverage.Complete = coverage.Complete && coverage.CandidateMessagesExact &&
		progressCount == coverage.CandidateMessages
	if budgetCandidate != nil && progressCount == 0 {
		return mail.SearchPage{}, &searchBudgetTooSmallError{
			requiredBytes: budgetRequiredBytes,
			maximumBytes:  prepared.Query.MaxBytes,
		}
	}
	page := mail.SearchPage{Messages: results, Coverage: coverage}
	var cursorItem *messageRecord
	cursorInclusive := false
	if budgetCandidate != nil {
		cursorItem = budgetCandidate
		cursorInclusive = true
	} else if coverage.CandidateMessages > progressCount || !coverage.CandidateMessagesExact {
		cursorItem = lastScanned
	}
	if cursorItem != nil {
		var err error
		if cursorInclusive {
			page.NextCursor, err = searchCursorForInclusive(
				*cursorItem, prepared.Fingerprint, s.storeUUID, prepared.IndexRevision,
			)
		} else {
			page.NextCursor, err = searchCursorFor(
				*cursorItem, prepared.Fingerprint, s.storeUUID, prepared.IndexRevision,
			)
		}
		return page, err
	}
	return page, nil
}

func (s *Store) querySearchRecords(
	ctx context.Context,
	plan searchPlan,
	limit int,
) (result []messageRecord, resultErr error) {
	query := `SELECT
		m.ROWID, COALESCE(m.message_id, 0), COALESCE(m.global_message_id, 0),
		COALESCE(m.remote_id, 0), COALESCE(m.remote_mailbox, 0),
		m.mailbox, mb.url,
		subject.subject, sender.address, sender.comment,
		COALESCE(summary.summary, ''), COALESCE(m.date_sent, 0), m.date_sent IS NULL,
		COALESCE(m.date_received, 0), m.date_received IS NULL, m.read, m.flagged, m.deleted,
		EXISTS (SELECT 1 FROM server_messages sm WHERE sm.message = m.ROWID AND sm.junk_level > 0),
		m.size, (SELECT count(*) FROM attachments attachment WHERE attachment.message = m.ROWID)
	` + plan.fromWhereSQL + " ORDER BY m.date_received DESC, m.ROWID DESC LIMIT ?"
	query = plan.prefixSQL + query
	arguments := append(append([]any(nil), plan.arguments...), limit)
	rows, err := s.database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query Envelope Index search candidates: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "search candidate rows")
	var items []messageRecord
	for rows.Next() {
		var item messageRecord
		if err := rows.Scan(
			&item.RowID, &item.StoreMessageID, &item.StoreGlobalID, &item.RemoteID, &item.RemoteMailboxID,
			&item.StoreMailboxID,
			&item.PhysicalURL,
			&item.Subject, &item.SenderAddress, &item.SenderName, &item.SummaryText,
			&item.DateSent, &item.DateSentNull, &item.DateReceived, &item.DateReceivedNull,
			&item.Read, &item.Flagged, &item.Deleted,
			&item.Junk, &item.Size, &item.AttachmentCount,
		); err != nil {
			return nil, fmt.Errorf("scan Envelope Index search candidate: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Envelope Index search candidates: %w", err)
	}
	return items, nil
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

// dispatchSearchJobs feeds the scan workers. It first pre-classifies
// attachment-only queries: a positive catalog count decides the candidate
// without opening the source, while catalog-zero candidates dispatch as
// normal because the catalog can lag.
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
		// Attachment-only queries decidable from the catalog skip the source
		// entirely: count > 0 proves a match for --attachment true and
		// disproves it for --attachment false (max(catalog, parts) is
		// monotone in catalog). No open, no scan, no byte budget.
		// Catalog-zero candidates fall through to the scan because the
		// catalog can lag behind a newly downloaded attachment.
		if len(terms) == 0 && hasAttachment != nil {
			if *hasAttachment && item.AttachmentCount > 0 {
				results[index] = candidateScan{
					match: true, attachments: item.AttachmentCount, processed: true,
					contentWhole: true, catalogProven: true,
				}
				continue
			}
			if !*hasAttachment && item.AttachmentCount > 0 {
				results[index] = candidateScan{
					match: false, attachments: item.AttachmentCount, processed: true,
					contentWhole: true, catalogProven: true,
				}
				continue
			}
		}
		source, unavailable, err := s.openSearchCandidate(ctx, item)
		if err != nil {
			return false, err
		}
		if source == nil {
			unavailable.processed = true
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
			results[index] = candidateScan{budgetLimited: true, requiredBytes: source.length}
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

func (s *Store) openSearchCandidate(ctx context.Context, item messageRecord) (*emlxSource, candidateScan, error) {
	location, err := parseMailboxURL(item.PhysicalURL)
	if err != nil {
		return nil, candidateScan{}, operationError("unsupported_mail_store_schema", err.Error())
	}
	messageRef, err := encodeMessageReference(item, location.AccountID, location.VisiblePath, s.storeUUID)
	if err != nil {
		return nil, candidateScan{}, fmt.Errorf("encode search candidate reference: %w", err)
	}
	// Re-resolve and revalidate the store-bound identity before opening the
	// source so a moved or replaced Envelope Index row cannot become a hit.
	_, source, err := s.openMessageSource(ctx, messageRef)
	if err != nil {
		if isUnavailableSearchSourceError(err) {
			return nil, candidateScan{missing: true}, nil
		}
		return nil, candidateScan{}, err
	}
	return source, candidateScan{}, nil
}

func isUnavailableSearchSourceError(err error) bool {
	var coded interface {
		error
		ErrorCode() string
	}
	if errors.As(err, &coded) {
		switch coded.ErrorCode() {
		case "message_source_missing", "invalid_emlx":
			return true
		}
	}
	return errors.Is(err, fs.ErrNotExist)
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
	document, err := parseMIMEDocumentWithContext(ctx, source.Reader(), source.partial, false, true)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return candidateScan{}, ctxErr
		}
		knownAttachmentMatch := len(terms) == 0 && hasAttachment != nil &&
			*hasAttachment && item.AttachmentCount > 0
		return candidateScan{
			match: knownAttachmentMatch, attachments: item.AttachmentCount,
			bytes: source.length, partial: source.partial, full: !source.partial,
			contentWhole: false, processed: true,
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
		representations := buildSearchTextRepresentations(item, document)
		firstTerm := ""
		if len(terms) > 0 {
			match, firstTerm = containsAllFoldedSearchTerms(representations.folded, terms)
		}
		if match {
			// Build original-case text only for the snippet to preserve
			// readable case in search results.
			snippet = snippetForSearchText(&representations, firstTerm)
		}
	}
	return candidateScan{
		match: match, snippet: snippet, bytes: source.length,
		attachments: attachmentCount,
		partial:     source.partial, full: !source.partial, contentWhole: document.Complete,
		processed: true,
	}, nil
}

func mergeSearchCoverage(coverage *mail.SearchCoverage, scan candidateScan) {
	if scan.full || scan.partial || scan.missing {
		coverage.ScannedMessages++
	}
	if scan.catalogProven {
		coverage.CatalogProvenMessages++
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
