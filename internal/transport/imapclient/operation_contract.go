package imapclient

// OperationClass identifies the externally meaningful kind of IMAP work.
type OperationClass string

const (
	OperationClassRead     OperationClass = "read"
	OperationClassMutation OperationClass = "mutation"
	OperationClassStatus   OperationClass = "status"
	OperationClassFetch    OperationClass = "fetch"
	OperationClassClose    OperationClass = "close"
)

// OperationConcurrency describes which gate an operation acquires.
type OperationConcurrency string

const (
	OperationConcurrencySharedAccount    OperationConcurrency = "shared_account"
	OperationConcurrencyExclusiveAccount OperationConcurrency = "exclusive_account"
	OperationConcurrencyExclusiveClient  OperationConcurrency = "exclusive_client"
)

// SessionOwnership describes the connection state owned by an operation.
type SessionOwnership string

const (
	SessionOwnershipPooled    SessionOwnership = "pooled"
	SessionOwnershipDedicated SessionOwnership = "dedicated"
	SessionOwnershipAllPooled SessionOwnership = "all_pooled"
)

// MailboxSelection describes how an operation treats selected-mailbox state.
type MailboxSelection string

const (
	MailboxSelectionNone          MailboxSelection = "none"
	MailboxSelectionReuseOrSelect MailboxSelection = "reuse_or_select"
	MailboxSelectionFresh         MailboxSelection = "fresh_select"
)

// UIDValidityContract describes an operation's UIDVALIDITY behavior.
type UIDValidityContract string

const (
	UIDValidityNotUsed       UIDValidityContract = "not_used"
	UIDValidityObserved      UIDValidityContract = "observed"
	UIDValidityRequiredMatch UIDValidityContract = "required_match"
)

// OperationContract is the machine-readable concurrency and session-state
// contract for one public Client operation.
type OperationContract struct {
	Operation        string               `json:"operation"`
	Class            OperationClass       `json:"class"`
	Concurrency      OperationConcurrency `json:"concurrency"`
	SessionOwnership SessionOwnership     `json:"session_ownership"`
	MailboxSelection MailboxSelection     `json:"mailbox_selection"`
	UIDValidity      UIDValidityContract  `json:"uidvalidity"`
}

var operationContracts = []OperationContract{
	{Operation: "LIST", Class: OperationClassRead, Concurrency: OperationConcurrencySharedAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionNone, UIDValidity: UIDValidityNotUsed},
	{Operation: "STATUS", Class: OperationClassStatus, Concurrency: OperationConcurrencySharedAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionNone, UIDValidity: UIDValidityObserved},
	{Operation: "SEARCH", Class: OperationClassRead, Concurrency: OperationConcurrencySharedAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionReuseOrSelect, UIDValidity: UIDValidityObserved},
	{Operation: "FETCH", Class: OperationClassFetch, Concurrency: OperationConcurrencySharedAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionFresh, UIDValidity: UIDValidityRequiredMatch},
	{Operation: "APPEND", Class: OperationClassMutation, Concurrency: OperationConcurrencyExclusiveAccount, SessionOwnership: SessionOwnershipDedicated, MailboxSelection: MailboxSelectionFresh, UIDValidity: UIDValidityNotUsed},
	{Operation: "STORE", Class: OperationClassMutation, Concurrency: OperationConcurrencyExclusiveAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionFresh, UIDValidity: UIDValidityRequiredMatch},
	{Operation: "COPY", Class: OperationClassMutation, Concurrency: OperationConcurrencyExclusiveAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionFresh, UIDValidity: UIDValidityRequiredMatch},
	{Operation: "MOVE", Class: OperationClassMutation, Concurrency: OperationConcurrencyExclusiveAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionFresh, UIDValidity: UIDValidityRequiredMatch},
	{Operation: "DELETE", Class: OperationClassMutation, Concurrency: OperationConcurrencyExclusiveAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionFresh, UIDValidity: UIDValidityRequiredMatch},
	{Operation: "CLOSE", Class: OperationClassClose, Concurrency: OperationConcurrencyExclusiveClient, SessionOwnership: SessionOwnershipAllPooled, MailboxSelection: MailboxSelectionNone, UIDValidity: UIDValidityNotUsed},
}

// OperationContracts returns an independent copy of the Client contract.
func OperationContracts() []OperationContract {
	return append([]OperationContract(nil), operationContracts...)
}
