package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Command errors are buffered for JSON classification, but an editor needs
// live diagnostics and the original terminal descriptor.
type commandDiagnosticBuffer struct {
	bytes.Buffer
	processOutput io.Writer
}

type draftEditorStreams struct {
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	jsonOutput bool
}

func editorProcessWriter(writer io.Writer) io.Writer {
	for {
		switch wrapped := writer.(type) {
		case *commandDiagnosticBuffer:
			writer = wrapped.processOutput
		case *errorTrackingWriter:
			writer = wrapped.writer
		default:
			return writer
		}
	}
}

func runDraftEditor(ctx context.Context, process *exec.Cmd, streams draftEditorStreams) error {
	process.Stdin = streams.stdin
	process.Stderr = editorProcessWriter(streams.stderr)
	process.Stdout = editorProcessWriter(streams.stdout)
	if streams.jsonOutput {
		process.Stdout = process.Stderr
	}
	restore, err := prepareDraftEditorTerminal(process)
	if err != nil {
		return &commandError{code: "editor_terminal_unavailable", message: err.Error()}
	}
	runErr := runOwnedProcess(process, 2*time.Second)
	if restore != nil {
		runErr = errors.Join(runErr, restore())
	}
	if ctx.Err() != nil || editorInterrupted(process.ProcessState) {
		return errors.Join(&commandError{code: "editor_canceled", message: "draft editor canceled"}, ctx.Err(), runErr)
	}
	if runErr != nil {
		return errors.Join(&commandError{code: "editor_failed", message: "draft editor failed"}, runErr)
	}
	return nil
}

func editorInterrupted(state *os.ProcessState) bool {
	if state == nil {
		return false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && (status.Signal() == syscall.SIGINT || status.Signal() == syscall.SIGTERM)
}

type draftEditorEvidence struct {
	Ref              string `json:"ref"`
	ExpectedRevision string `json:"expected_revision"`
	CandidatePath    string `json:"candidate_path"`
	ExitCode         *int   `json:"exit_code,omitempty"`
	Signal           string `json:"signal,omitempty"`
}

type draftEditorError struct {
	cause           error
	evidence        draftEditorEvidence
	updateAttempted bool
}

func newDraftEditorError(cause error, ref, revision, path string, state *os.ProcessState, updateAttempted bool) error {
	evidence := draftEditorEvidence{Ref: ref, ExpectedRevision: revision, CandidatePath: path}
	if state != nil {
		code := state.ExitCode()
		evidence.ExitCode = &code
		if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			evidence.Signal = status.Signal().String()
		}
	}
	return &draftEditorError{cause: cause, evidence: evidence, updateAttempted: updateAttempted}
}

func (e *draftEditorError) Error() string {
	return fmt.Sprintf("%s; editor candidate retained at %q", e.cause, e.evidence.CandidatePath)
}

func (e *draftEditorError) Unwrap() error { return e.cause }
