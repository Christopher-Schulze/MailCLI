package mail

import "errors"

// DraftReconcileRequiresMailStore reports whether the selected retained send
// claim needs the local Mail store's observation baseline. It only reads the
// private draft state and never creates the draft directory, opens SQLite, or
// contacts Mail.app. Invalid or incomplete state is conservative and keeps
// the store-backed path enabled.
func DraftReconcileRequiresMailStore(ref string) bool {
	root, err := defaultDraftRoot()
	if err != nil {
		return true
	}
	draft, err := loadDraftDocument(root, ref)
	if err != nil {
		if !isDraftNotFound(err) {
			return true
		}
		return !hasActiveSendReceipt(root, ref)
	}
	if err := attachDraftAttempts(root, ref, &draft); err != nil {
		return true
	}
	if draft.SendAttempt == nil {
		return !hasActiveSendReceipt(root, ref)
	}
	switch draft.SendAttempt.Outcome {
	case SendOutcomeObserved, SendOutcomeSent, SendOutcomeMirrorPending:
		return false
	default:
		return draft.SendAttempt.ObservationBaseline != nil
	}
}

func hasActiveSendReceipt(root string, ref string) bool {
	receipt, err := readActiveSendReceipt(root, ref)
	return err == nil && receipt != nil
}

func isDraftNotFound(err error) bool {
	var operation *OperationError
	return errors.As(err, &operation) && operation.Code == "not_found"
}
