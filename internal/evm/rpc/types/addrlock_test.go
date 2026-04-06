package types

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func TestAddrLockerLockReuseAndBlocking(t *testing.T) {
	var locker AddrLocker
	addr := common.HexToAddress("0x1000000000000000000000000000000000000001")

	first := locker.lock(addr)
	second := locker.lock(addr)
	if first != second {
		t.Fatal("expected identical mutex instance for same address")
	}

	locker.LockAddr(addr)
	acquired := make(chan struct{})
	go func() {
		locker.LockAddr(addr)
		close(acquired)
		locker.UnlockAddr(addr)
	}()

	select {
	case <-acquired:
		t.Fatal("lock should block until unlocked")
	case <-time.After(50 * time.Millisecond):
	}

	locker.UnlockAddr(addr)

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("expected waiting goroutine to acquire lock after unlock")
	}
}
