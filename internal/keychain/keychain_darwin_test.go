//go:build darwin && cgo

package keychain

import (
	"testing"
)

func TestCFStringAndCFDataWithoutKeychain(t *testing.T) {
	t.Parallel()

	cf, err := cfString("mailcli-test")
	if err != nil {
		t.Fatalf("cfString() error = %v", err)
	}
	if cf == 0 {
		t.Fatal("cfString() = 0")
	}
	releaseCFString(cf)

	data, err := cfData("secret")
	if err != nil {
		t.Fatalf("cfData() error = %v", err)
	}
	if data == 0 {
		t.Fatal("cfData() = 0")
	}
	releaseCFData(data)
}
