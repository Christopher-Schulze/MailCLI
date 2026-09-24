package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"mailcli/internal/mail"
)

type attachmentSaveResult struct {
	saved   mail.SavedAttachment
	err     error
	request mail.SaveAttachmentRequest
}

type attachmentSaveOutcomeCase struct {
	name          string
	certainty     mail.EffectCertainty
	replayAllowed bool
	saved         bool
	cause         error
}

func (saver *attachmentSaveResult) SaveAttachment(
	_ context.Context,
	request mail.SaveAttachmentRequest,
) (mail.SavedAttachment, error) {
	saver.request = request
	return saver.saved, saver.err
}

func TestRunAttachmentsSaveRetainsOutcomeEvidence(t *testing.T) {
	cases := []attachmentSaveOutcomeCase{
		{name: "before publish", certainty: mail.EffectNone, replayAllowed: true, cause: context.DeadlineExceeded},
		{name: "after publish", certainty: mail.EffectComplete, saved: true, cause: errors.New("close failed")},
		{name: "ambiguous cleanup", certainty: mail.EffectUnknown, cause: &mail.OperationError{Code: "attachment_changed", Message: "output changed"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) { runAttachmentSaveOutcomeCase(t, test) })
	}
}

func runAttachmentSaveOutcomeCase(t *testing.T, test attachmentSaveOutcomeCase) {
	t.Helper()
	outputPath := filepath.Join(t.TempDir(), "attachment.bin")
	saved := mail.SavedAttachment{}
	if test.saved {
		saved = mail.SavedAttachment{
			AttachmentID: "1", Path: outputPath, Size: 8,
			SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		}
	}
	err := &mail.AttachmentSaveOutcomeError{Cause: test.cause, Phase: mail.OperationPhaseCleanup, EffectCertainty: test.certainty}
	if test.certainty == mail.EffectNone {
		err.Phase = mail.OperationPhaseExecution
	}
	saver := &attachmentSaveResult{saved: saved, err: err}
	var stdout, stderr bytes.Buffer
	code := runAttachmentsSave(context.Background(), saver, []string{
		"--message", "message", "--attachment", "1", "--output", outputPath, "--json",
	}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("runAttachmentsSave() = (%d, %q, %q), want JSON operation failure", code, stdout.String(), stderr.String())
	}
	if saver.request.MessageRef != "message" || saver.request.AttachmentID != "1" || saver.request.OutputPath != outputPath {
		t.Fatalf("SaveAttachment() request = %+v", saver.request)
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	assertAttachmentSaveOutcomeResponse(t, test, response, saved, outputPath)
}

func assertAttachmentSaveOutcomeResponse(
	t *testing.T,
	test attachmentSaveOutcomeCase,
	response envelope,
	saved mail.SavedAttachment,
	outputPath string,
) {
	t.Helper()
	if response.OK || response.Error == nil || response.Error.Guidance == nil {
		t.Fatalf("response = %+v, want failed envelope with guidance", response)
	}
	guidance := response.Error.Guidance
	if guidance.EffectCertainty != test.certainty || guidance.ReplayAllowed != test.replayAllowed {
		t.Fatalf("guidance = %+v, want certainty %q and replay %t", guidance, test.certainty, test.replayAllowed)
	}
	if test.saved {
		if response.Data.SavedAttachment == nil || response.Data.SavedAttachment.Path != outputPath ||
			response.Data.SavedAttachment.Size != saved.Size || response.Data.SavedAttachment.SHA256 != saved.SHA256 {
			t.Fatalf("saved attachment = %+v, want %+v", response.Data.SavedAttachment, saved)
		}
	} else if response.Data.SavedAttachment != nil {
		t.Fatalf("saved attachment = %+v, want no verified evidence", response.Data.SavedAttachment)
	}
}
