package blocksync

import (
	"context"
	"testing"
	"time"

	"github.com/pkg/errors"
)

func TestLowestAvailableHeight(t *testing.T) {
	t.Run("parses rpc error payload", func(t *testing.T) {
		err := errors.New(`{"jsonrpc":"2.0","id":-1,"error":{"code":-32603,"message":"Internal error","data":"height 45445 is not available, lowest height is 106890001"}}`)

		height, ok := LowestAvailableHeight(err)
		if !ok {
			t.Fatal("expected lowest available height to be parsed")
		}
		if height != 106890001 {
			t.Fatalf("expected height 106890001, got %d", height)
		}
	})

	t.Run("returns false when height is absent", func(t *testing.T) {
		height, ok := LowestAvailableHeight(errors.New("some other rpc failure"))
		if ok {
			t.Fatalf("expected parse failure, got height %d", height)
		}
	})
}

func TestIsAheadOfChainHead(t *testing.T) {
	err := errors.New(`{"jsonrpc":"2.0","id":-1,"error":{"code":-32603,"message":"Internal error","data":"height 45445 must be less than or equal to the current blockchain height 45444"}}`)
	if !isAheadOfChainHead(err) {
		t.Fatal("expected ahead-of-head error to be detected")
	}

	if isAheadOfChainHead(errors.New("some other rpc failure")) {
		t.Fatal("expected non-head error to be ignored")
	}
}

func TestFetchWithRetryKeepsWaitingForAheadOfHead(t *testing.T) {
	getter := &blockGetter{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var calls int
	err := getter.fetchWithRetry(ctx, 0, 45445, "block", func() error {
		calls++
		if calls <= retryAttempts+1 {
			return errors.New(`{"jsonrpc":"2.0","id":-1,"error":{"code":-32603,"message":"Internal error","data":"height 45445 must be less than or equal to the current blockchain height 45444"}}`)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected ahead-of-head retries to eventually succeed, got %v", err)
	}
	if calls != retryAttempts+2 {
		t.Fatalf("unexpected call count: got %d want %d", calls, retryAttempts+2)
	}
}
