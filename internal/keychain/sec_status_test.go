package keychain

import (
	"testing"

	"mailcli/internal/transport"
)

func TestMapAddStatus(t *testing.T) {
	t.Parallel()

	if err := mapAddStatus(secSuccess); err != nil {
		t.Fatalf("success error = %v", err)
	}
	err := mapAddStatus(secDuplicateItem)
	if err == nil {
		t.Fatal("duplicate error = nil")
	}
	if code := transport.ErrorCode(err); code != CodeDuplicate {
		t.Errorf("duplicate code = %q, want %q", code, CodeDuplicate)
	}
	err = mapAddStatus(-1)
	if err == nil {
		t.Fatal("failed add error = nil")
	}
	if code := transport.ErrorCode(err); code != CodeStoreFailed {
		t.Errorf("failed add code = %q, want %q", code, CodeStoreFailed)
	}
}

func TestMapUpdateStatus(t *testing.T) {
	t.Parallel()

	if err := mapUpdateStatus(secSuccess); err != nil {
		t.Fatalf("success error = %v", err)
	}
	err := mapUpdateStatus(secItemNotFound)
	if transport.ErrorCode(err) != CodeNotFound {
		t.Errorf("not found code = %q", transport.ErrorCode(err))
	}
	err = mapUpdateStatus(-2)
	if transport.ErrorCode(err) != CodeStoreFailed {
		t.Errorf("failed update code = %q", transport.ErrorCode(err))
	}
}

func TestMapFindStatus(t *testing.T) {
	t.Parallel()

	if err := mapFindStatus(secSuccess); err != nil {
		t.Fatalf("success error = %v", err)
	}
	if transport.ErrorCode(mapFindStatus(secItemNotFound)) != CodeNotFound {
		t.Fatal("find missing want not found")
	}
	if transport.ErrorCode(mapFindStatus(-3)) != CodeLoadFailed {
		t.Fatal("find fail want load failed")
	}
}

func TestMapDeleteStatus(t *testing.T) {
	t.Parallel()

	if err := mapDeleteStatus(secSuccess); err != nil {
		t.Fatalf("success error = %v", err)
	}
	if transport.ErrorCode(mapDeleteStatus(secItemNotFound)) != CodeNotFound {
		t.Fatal("delete missing want not found")
	}
	if transport.ErrorCode(mapDeleteStatus(-4)) != CodeDeleteFailed {
		t.Fatal("delete fail want delete failed")
	}
}
