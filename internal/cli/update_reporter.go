package cli

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type updateReporter struct {
	writer   io.Writer
	enabled  bool
	animated bool
	colored  bool
}

func newUpdateReporter(writer io.Writer, enabled bool, animated bool) *updateReporter {
	_, noColor := os.LookupEnv("NO_COLOR")
	animated = enabled && animated && os.Getenv("TERM") != "dumb"
	return &updateReporter{writer: writer, enabled: enabled, animated: animated, colored: animated && !noColor}
}

func (r *updateReporter) paint(color, text string) string {
	if r.colored {
		return color + text + "\x1b[0m"
	}
	return text
}

func (r *updateReporter) result(result updateResult) {
	if !r.enabled {
		return
	}
	message := fmt.Sprintf("Already up to date (mailcli %s).", result.CurrentVersion)
	mark, color := "✓", "\x1b[32m"
	switch {
	case result.Updated:
		message = fmt.Sprintf("Updated mailcli from %s to %s.", result.CurrentVersion, result.LatestVersion)
		if r.animated {
			message = fmt.Sprintf("Updated mailcli  %s → %s", result.CurrentVersion, result.LatestVersion)
		}
	case result.UpdateAvailable != nil && *result.UpdateAvailable:
		message = fmt.Sprintf("Update available: mailcli %s -> %s; run `mailcli update` to install it.", result.CurrentVersion, result.LatestVersion)
		mark, color = "→", "\x1b[36m"
	}
	if r.animated {
		writeFormat(r.writer, "\n  %s %s\n\n", r.paint(color, mark), message)
		return
	}
	writeLine(r.writer, message)
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
	for index := range frames {
		frames[index] = r.paint("\x1b[36m", frames[index])
	}
	done := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	writeFormat(r.writer, "\r  %s %s...", frames[0], message)
	go animateUpdateStatus(r.writer, message, frames, done, &wait)
	err := action()
	close(done)
	wait.Wait()
	if err != nil {
		writeFormat(r.writer, "\r\x1b[2K  %s %s failed.\n", r.paint("\x1b[31m", "✗"), message)
		return err
	}
	writeFormat(r.writer, "\r\x1b[2K  %s %s.\n", r.paint("\x1b[32m", "✓"), message)
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
			writeFormat(writer, "\r  %s %s...", frames[frameIndex%len(frames)], message)
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
