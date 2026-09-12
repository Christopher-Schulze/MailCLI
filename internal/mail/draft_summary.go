package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"syscall"
)

func readDraftSummary(ctx context.Context, ref string, state *draftStorage) (DraftSummary, error) {
	if _, err := draftPath(state.rootName, ref); err != nil {
		return DraftSummary{}, err
	}
	draft, err := readDraftSummaryRecord[Draft](ctx, ref+".json", state)
	if err != nil {
		return DraftSummary{}, err
	}
	if draft.Ref != ref {
		return DraftSummary{}, errors.New("draft reference does not match its file")
	}
	if draft.BodyFormat == "" {
		draft.BodyFormat = DraftBodyPlain
	}
	draft.SendAttempt, draft.SaveAttempt, draft.HandoffAttempt = nil, nil, nil
	if err := attachDraftSummaryAttempts(ctx, ref, &draft, state); err != nil {
		return DraftSummary{}, err
	}
	return draftSummaryFrom(draft), nil
}

func attachDraftSummaryAttempts(ctx context.Context, ref string, draft *Draft, state *draftStorage) error {
	send, err := readDraftSummaryRecord[storedSendAttempt](ctx, ref+".send-claim", state)
	if err == nil {
		if !validSendAttempt(send, ref) {
			return errors.New("send claim is invalid")
		}
		draft.SendAttempt = &send.Attempt
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	save, err := readDraftSummaryRecord[storedDraftSaveAttempt](ctx, ref+".save-claim", state)
	if err == nil {
		if !validDraftSaveAttempt(save, ref) {
			return errors.New("draft-save claim is invalid")
		}
		draft.SaveAttempt = &save.Attempt
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	handoff, err := readDraftSummaryRecord[storedHandoffAttempt](ctx, ref+handoffClaimSuffix, state)
	if err == nil {
		if !validHandoffAttempt(handoff, ref) {
			return errors.New("handoff claim is invalid")
		}
		draft.HandoffAttempt = &handoff.Attempt
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if draft.SendAttempt != nil && draft.SaveAttempt != nil ||
		draft.HandoffAttempt != nil && (draft.SendAttempt != nil || draft.SaveAttempt != nil) {
		return errors.New("draft has conflicting operation claims")
	}
	return nil
}

func readDraftSummaryRecord[T Draft | storedSendAttempt | storedDraftSaveAttempt | storedHandoffAttempt](
	ctx context.Context, name string, state *draftStorage,
) (T, error) {
	var record T
	payload, err := readDraftSummaryProjection(ctx, name, state)
	if err != nil {
		return record, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, errors.New("draft record has invalid JSON fields or structure")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return record, errors.New("draft record must contain exactly one JSON object")
	}
	return record, nil
}

func readDraftSummaryProjection(ctx context.Context, name string, state *draftStorage) (result []byte, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	expected, err := state.lstat(name)
	if err != nil {
		return nil, err
	}
	if !expected.Mode().IsRegular() || expected.Size() > maximumDraftStateBytes {
		return nil, errors.New("draft record is not a bounded regular file")
	}
	file, opened, err := state.openFile(name, expected, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if !opened.Mode().IsRegular() || opened.Size() != expected.Size() || !opened.ModTime().Equal(expected.ModTime()) {
		return nil, errors.New("draft record changed while opening")
	}
	projection, _, err := projectDraftSummaryJSON(ctx, file, opened.Size())
	if err != nil {
		return nil, err
	}
	current, err := state.lstat(name)
	if err != nil || !os.SameFile(expected, current) || current.Size() != expected.Size() || !current.ModTime().Equal(expected.ModTime()) {
		return nil, errors.New("draft record changed while reading")
	}
	return projection, nil
}
