package mail

import stdmail "net/mail"

// MailboxAddrSpec serializes a parsed net/mail Address.Address identity as
// mailbox text, restoring necessary local-part quoting without a display name
// or angle brackets. Callers must parse external text first; this formatter
// does not validate or repair input. Case and decoded mailbox identity survive
// parsing the result again.
func MailboxAddrSpec(identity string) string {
	encoded := (&stdmail.Address{Address: identity}).String()
	return encoded[1 : len(encoded)-1]
}
