package blocksync

import (
	"errors"
	"testing"
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
