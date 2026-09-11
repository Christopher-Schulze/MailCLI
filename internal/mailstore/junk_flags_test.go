package mailstore

import (
	"context"
	"reflect"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func TestMarkMessageJunkTransitions(t *testing.T) {
	for _, initial := range []struct {
		name  string
		flags []string
	}{
		{"neither", nil}, {"junk", []string{"$Junk"}},
		{"not junk", []string{"$NotJunk"}}, {"both", []string{"$Junk", "$NotJunk"}},
	} {
		for _, desired := range []struct {
			name string
			junk bool
			flag string
		}{{"mark junk", true, "$Junk"}, {"mark not junk", false, "$NotJunk"}} {
			t.Run(initial.name+"/"+desired.name, func(t *testing.T) {
				preserved := []string{"\\Seen", "\\Answered", "\\Flagged", "custom"}
				if initial.name == "both" {
					preserved = append(preserved, "Junk", "NotJunk")
				}
				operator := &stubImapOperator{flags: append(append([]string(nil), preserved...), initial.flags...)}
				client, original := flagResultFixture(t, operator)
				state, err := client.MarkMessage(context.Background(), mail.MarkMessageRequest{Ref: original.Ref, Junk: &desired.junk, AllowDraftMutation: true})
				wanted := append(append([]string(nil), preserved...), desired.flag)
				if err != nil || state.ServerTruth == nil || !reflect.DeepEqual(state.ServerTruth.ActualFlags, wanted) || state.Junk != desired.junk || !state.Read || !state.Flagged {
					t.Fatalf("junk transition = %+v, %v; wanted flags %v", state, err, wanted)
				}
				if operator.mutationCalls != 1 {
					t.Fatalf("mark retried the mutation: %d calls", operator.mutationCalls)
				}
			})
		}
	}
}

func TestMarkMessageJunkObservationClassification(t *testing.T) {
	for _, test := range []struct {
		name  string
		flags []string
		junk  bool
	}{
		{"standard junk", []string{"$jUnK"}, true},
		{"standard not junk", []string{"$NotJunk"}, false},
		{"conflicting standardized pair", []string{"$Junk", "$NOTJUNK"}, false},
		{"legacy values are not standardized classification", []string{"Junk", "NotJunk"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			operator := &flagResultOperator{stubImapOperator: &stubImapOperator{}, results: []flagBoundaryResult{{evidence: transport.MutationEvidence{Outcome: transport.MutationOutcomeCompleted, FlagsState: transport.FlagObservationObserved, ActualFlags: test.flags, FlagsSource: "STORE"}}}}
			client, original := flagResultFixture(t, operator)
			flagged := false
			state, err := client.MarkMessage(context.Background(), mail.MarkMessageRequest{Ref: original.Ref, Flagged: &flagged, AllowDraftMutation: true})
			if err != nil || state.Junk != test.junk || state.ServerTruth == nil || !reflect.DeepEqual(state.ServerTruth.ActualFlags, test.flags) {
				t.Fatalf("classification = %+v, %v; wanted junk %t and flags %v", state, err, test.junk, test.flags)
			}
		})
	}
}
