package imapclient

// responseBudget separates explicitly requested literal payloads from the
// shared protocol metadata cap. Commands without a payload allowance charge
// literals, including STATUS mailbox names, to metadata too.
type responseBudget struct {
	metadataBytes int64
	payloadBytes  int64
	payloadLimit  int64
}

func (b *responseBudget) consume(sess *session, size int64, literal bool) error {
	if b == nil {
		return nil
	}
	used, limit, name := &b.metadataBytes, int64(maxFlagResponseBytes), "command response metadata bytes"
	if literal && b.payloadLimit > 0 {
		used, limit, name = &b.payloadBytes, b.payloadLimit, "command response payload bytes"
	}
	if size > limit-*used {
		return completionResponseLimitExceeded(sess, name, limit)
	}
	*used += size
	return nil
}
