package mailstore

import (
	"context"
	"fmt"
)

// candidateChunkSize bounds how many body-search candidate records are
// resident at once. Memory becomes O(page + chunk) instead of
// O(candidates); the keyset tuple (date_received, ROWID) avoids OFFSET
// rescans while keeping the cursor ordering.
const candidateChunkSize = 512

// searchCandidateStream yields body-search candidates in chunks using
// keyset pagination on (date_received, ROWID). The first chunk uses the
// plan's own filters; later chunks add the strictly-below tuple predicate.
type searchCandidateStream struct {
	store    *Store
	ctx      context.Context
	plan     searchPlan
	last     messageRecord
	lastNull bool
	haveLast bool
	done     bool
}

func (s *Store) newSearchCandidateStream(ctx context.Context, plan searchPlan) *searchCandidateStream {
	return &searchCandidateStream{store: s, ctx: ctx, plan: plan}
}

// next returns the next chunk of candidates. Chunk queries stay keyset-only;
// callers derive default coverage from observed rows and stream exhaustion.
func (st *searchCandidateStream) next(limit int) (chunk []messageRecord, resultErr error) {
	if st.done {
		return nil, nil
	}
	query := `SELECT
		m.ROWID, COALESCE(m.message_id, 0), COALESCE(m.global_message_id, 0),
		COALESCE(m.remote_id, 0), COALESCE(m.remote_mailbox, 0),
		m.mailbox, mb.url,
		subject.subject, sender.address, sender.comment,
		COALESCE(summary.summary, ''), COALESCE(m.date_sent, 0), m.date_sent IS NULL,
		COALESCE(m.date_received, 0), m.date_received IS NULL, m.read, m.flagged, m.deleted,
		EXISTS (SELECT 1 FROM server_messages sm WHERE sm.message = m.ROWID AND sm.junk_level > 0),
		m.size, (SELECT count(*) FROM attachments attachment WHERE attachment.message = m.ROWID)
	`
	arguments := append([]any(nil), st.plan.arguments...)
	if st.haveLast {
		// Keyset on the RAW (m.date_received, m.ROWID) DESC order the query
		// uses: NULL dates sort last in SQLite DESC, so a non-NULL cursor
		// must also admit the trailing NULL rows, and a NULL cursor only
		// continues inside the NULL group by ROWID.
		if st.lastNull {
			query += st.plan.fromWhereSQL +
				" AND m.date_received IS NULL AND m.ROWID < ?" +
				" ORDER BY m.date_received DESC, m.ROWID DESC LIMIT ?"
			arguments = append(arguments, st.last.RowID, limit)
		} else {
			query += st.plan.fromWhereSQL +
				" AND ((m.date_received < ? OR (m.date_received = ? AND m.ROWID < ?)) OR m.date_received IS NULL)" +
				" ORDER BY m.date_received DESC, m.ROWID DESC LIMIT ?"
			arguments = append(arguments,
				st.last.DateReceived, st.last.DateReceived, st.last.RowID, limit,
			)
		}
	} else {
		query += st.plan.fromWhereSQL + " ORDER BY m.date_received DESC, m.ROWID DESC LIMIT ?"
		arguments = append(arguments, limit)
	}
	query = st.plan.prefixSQL + query
	rows, err := st.store.database.QueryContext(st.ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query Envelope Index search candidates: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "search candidate rows")
	var items []messageRecord
	lastDateNull := false
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
		// Only the chunk's last row seeds the next keyset cursor; preserve
		// its raw NULL state independently of the coalesced timestamp.
		lastDateNull = item.DateReceivedNull
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Envelope Index search candidates: %w", err)
	}
	if len(items) == 0 {
		st.done = true
		return nil, nil
	}
	st.store.searchCandidateRowsLoaded.Add(int64(len(items)))
	st.last = items[len(items)-1]
	st.lastNull = lastDateNull
	st.haveLast = true
	if len(items) < limit {
		st.done = true
	}
	return items, nil
}

// countSearchCandidates returns an exact total only when the candidate set is
// within limit. The inner LIMIT bounds explicit exact-count work and lets the
// caller fail closed instead of presenting a truncated value as exact.
func (s *Store) countSearchCandidates(ctx context.Context, plan searchPlan, limit int) (int, error) {
	query := "SELECT count(*) FROM (SELECT 1 " + plan.fromWhereSQL + " LIMIT ?)"
	query = plan.prefixSQL + query
	arguments := append(append([]any(nil), plan.arguments...), limit+1)
	s.searchCandidateCountQueries.Add(1)
	rows, err := s.database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return 0, fmt.Errorf("count Envelope Index search candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return 0, fmt.Errorf("count Envelope Index search candidates: no row")
	}
	var total int
	if err := rows.Scan(&total); err != nil {
		return 0, fmt.Errorf("scan Envelope Index candidate count: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate Envelope Index candidate count: %w", err)
	}
	if total > limit {
		return 0, operationError(
			"search_count_limit_exceeded",
			fmt.Sprintf("exact candidate count exceeds max-messages %d; narrow the search or raise the bounded limit", limit),
		)
	}
	return total, nil
}
