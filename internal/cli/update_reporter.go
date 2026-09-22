package cli

import (
	"io"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type updateReporter struct {
	writer   io.Writer
	enabled  bool
	animated bool
}

func newUpdateReporter(writer io.Writer, enabled bool, animated bool) *updateReporter {
	return &updateReporter{writer: writer, enabled: enabled, animated: animated}
}

func (r *updateReporter) step(message string, action func() error) error {
	if !r.enabled {
		return action()
	}
	if !r.animated {
		writeLine(r.writer, message+"...")
		return action()
	}
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	done := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	writeFormat(r.writer, "\r%s %s...", frames[0], message)
	go animateUpdateStatus(r.writer, message, frames, done, &wait)
	err := action()
	close(done)
	wait.Wait()
	if err != nil {
		writeFormat(r.writer, "\r\x1b[2K✗ %s failed.\n", message)
		return err
	}
	writeFormat(r.writer, "\r\x1b[2K✓ %s.\n", message)
	return nil
}

func animateUpdateStatus(
	writer io.Writer,
	message string,
	frames []string,
	done <-chan struct{},
	wait *sync.WaitGroup,
) {
	defer wait.Done()
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	frameIndex := 1
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			writeFormat(writer, "\r%s %s...", frames[frameIndex%len(frames)], message)
			frameIndex++
		}
	}
}

func writerIsTerminal(writer io.Writer) bool {
	fileDescriptor, ok := writerFileDescriptor(writer)
	if !ok {
		return false
	}
	_, err := unix.IoctlGetTermios(int(fileDescriptor), unix.TIOCGETA)
	return err == nil
}

func writerFileDescriptor(writer io.Writer) (uintptr, bool) {
	switch value := writer.(type) {
	case interface{ Fd() uintptr }:
		return value.Fd(), true
	case *errorTrackingWriter:
		return writerFileDescriptor(value.writer)
	case *countingWriter:
		return writerFileDescriptor(value.writer)
	default:
		return 0, false
	}
}
