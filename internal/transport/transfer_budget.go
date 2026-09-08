package transport

import "time"

const (
	// TransferCommandBudget bounds short protocol commands and final replies.
	TransferCommandBudget = 30 * time.Second
	// TransferBandwidthFloorBytesPerSecond is the conservative throughput floor
	// used to size encoded message transfers.
	TransferBandwidthFloorBytesPerSecond int64 = 1 << 20
	// TransferBudgetCap bounds an encoded message transfer even for large spools.
	TransferBudgetCap = 15 * time.Minute
)

// TransferBudgetForSize returns the bounded budget for an encoded message
// transfer. Non-positive sizes retain the short command budget. The division
// happens before converting to time.Duration, and the cap check prevents any
// duration multiplication from overflowing for large caller-provided sizes.
func TransferBudgetForSize(size int64) time.Duration {
	if size <= 0 {
		return TransferCommandBudget
	}
	if TransferBudgetCap <= TransferCommandBudget {
		return TransferBudgetCap
	}
	maxExtraSeconds := int64((TransferBudgetCap - TransferCommandBudget) / time.Second)
	seconds := size / TransferBandwidthFloorBytesPerSecond
	if seconds >= maxExtraSeconds {
		return TransferBudgetCap
	}
	return TransferCommandBudget + time.Duration(seconds)*time.Second
}
