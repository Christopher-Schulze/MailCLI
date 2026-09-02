package mailapp

import "testing"

func TestUncertainMailStateError(t *testing.T) {
	err := &uncertainMailStateError{}
	want := "a previous Mail.app operation timed out and may still be running"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestInvalidAccessGateStateError(t *testing.T) {
	err := &invalidAccessGateStateError{}
	want := "Mail.app access gate recovery state is invalid"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestMailNotRunningError(t *testing.T) {
	err := &mailNotRunningError{}
	want := "Mail.app is not running"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
