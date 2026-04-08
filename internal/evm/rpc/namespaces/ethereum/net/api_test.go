package net

import (
	"testing"

	"github.com/cosmos/cosmos-sdk/client"
)

func TestNewPublicAPIOfflineClientContext(t *testing.T) {
	api := NewPublicAPI(client.Context{}.WithChainID("injective-1"))
	if api == nil {
		t.Fatal("expected api instance")
	}
	if got := api.Version(); got != "1" {
		t.Fatalf("unexpected network version: got %s want 1", got)
	}
	if api.Listening() {
		t.Fatal("expected offline api to report not listening")
	}
	if peers := api.PeerCount(); peers != 0 {
		t.Fatalf("unexpected peer count: got %d want 0", peers)
	}
}
