package backend

import (
	"io"
	"log/slog"
	"testing"

	"github.com/cosmos/cosmos-sdk/client"

	appconfig "github.com/InjectiveLabs/evm-gateway/internal/config"
)

func TestNewBackendUsesExplicitEVMChainID(t *testing.T) {
	b := NewBackend(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		appconfig.Config{
			ChainID:    "injective-1",
			EVMChainID: "1776",
		},
		client.Context{}.WithChainID("injective-1"),
		false,
		nil,
		nil,
	)

	if got := b.ChainID().ToInt().String(); got != "1776" {
		t.Fatalf("unexpected backend chain id: got %s want 1776", got)
	}
}
