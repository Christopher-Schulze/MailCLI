package mailstore

import (
	"mailcli/internal/mail"
)

func searchCursorFor(item messageRecord, fingerprint string, storeUUID string, indexRevision string) (string, error) {
	return mail.EncodeSearchCursorWithRevision(
		fingerprint, storeUUID, indexRevision, item.DateReceived, item.DateReceivedNull, item.RowID,
	)
}

func searchCursorForInclusive(
	item messageRecord,
	fingerprint string,
	storeUUID string,
	indexRevision string,
) (string, error) {
	return mail.EncodeSearchCursorInclusiveWithRevision(
		fingerprint, storeUUID, indexRevision, item.DateReceived, item.DateReceivedNull, item.RowID,
	)
}

func emptySearchPage(sourceScan bool) mail.SearchPage {
	backend := "envelope_sql"
	if sourceScan {
		backend = "emlx_stream"
	}
	return mail.SearchPage{
		Messages: []mail.SearchMessage{},
		Coverage: mail.SearchCoverage{Backend: backend, CandidateMessagesExact: true, Complete: true},
	}
}
