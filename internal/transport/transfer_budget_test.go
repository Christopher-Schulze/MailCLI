package transport

import (
	"math"
	"testing"
	"time"
)

func TestTransferBudgetForSize(t *testing.T) {
	maxExtraSeconds := int64((TransferBudgetCap - TransferCommandBudget) / time.Second)
	cases := []struct {
		name string
		size int64
		want time.Duration
	}{
		{name: "negative", size: -1, want: TransferCommandBudget},
		{name: "empty", size: 0, want: TransferCommandBudget},
		{name: "one byte", size: 1, want: TransferCommandBudget},
		{name: "one floor unit", size: TransferBandwidthFloorBytesPerSecond, want: TransferCommandBudget + time.Second},
		{name: "just below cap", size: (maxExtraSeconds - 1) * TransferBandwidthFloorBytesPerSecond, want: TransferCommandBudget + time.Duration(maxExtraSeconds-1)*time.Second},
		{name: "at cap", size: maxExtraSeconds * TransferBandwidthFloorBytesPerSecond, want: TransferBudgetCap},
		{name: "maximum int64", size: math.MaxInt64, want: TransferBudgetCap},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := TransferBudgetForSize(test.size); got != test.want {
				t.Fatalf("TransferBudgetForSize(%d) = %v, want %v", test.size, got, test.want)
			}
		})
	}
}

func TestTransferBudgetForSizeNeverExceedsCap(t *testing.T) {
	for _, size := range []int64{0, 1, TransferBandwidthFloorBytesPerSecond, math.MaxInt64} {
		if got := TransferBudgetForSize(size); got > TransferBudgetCap {
			t.Fatalf("TransferBudgetForSize(%d) = %v, exceeds cap %v", size, got, TransferBudgetCap)
		}
	}
}
