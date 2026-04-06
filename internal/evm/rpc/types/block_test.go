package types

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"testing"

	grpctypes "github.com/cosmos/cosmos-sdk/types/grpc"
	"github.com/ethereum/go-ethereum/common"
	"google.golang.org/grpc/metadata"
)

func TestNewBlockNumberAndContextWithHeight(t *testing.T) {
	overflow := new(big.Int).Add(big.NewInt(math.MaxInt64), big.NewInt(1))
	if got := NewBlockNumber(overflow); got != EthLatestBlockNumber {
		t.Fatalf("expected overflow block number to map to latest, got %d", got)
	}
	if got := NewBlockNumber(big.NewInt(15)); got != BlockNumber(15) {
		t.Fatalf("unexpected block number: %d", got)
	}

	type ctxKey string
	key := ctxKey("keep")
	parent := context.WithValue(context.Background(), key, "keep")
	ctx := ContextWithHeightFrom(parent, 42)
	if got := ctx.Value(key); got != "keep" {
		t.Fatalf("expected parent context value to be preserved, got %v", got)
	}
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok || len(md.Get(grpctypes.GRPCBlockHeightHeader)) != 1 || md.Get(grpctypes.GRPCBlockHeightHeader)[0] != "42" {
		t.Fatalf("unexpected outgoing metadata: %#v", md)
	}

	ctx = ContextWithHeightFrom(parent, 0)
	if ctx.Value(key) != "keep" {
		t.Fatalf("expected original parent context to be returned")
	}

	md, ok = metadata.FromOutgoingContext(ContextWithHeight(7))
	if !ok || md.Get(grpctypes.GRPCBlockHeightHeader)[0] != "7" {
		t.Fatalf("unexpected metadata from ContextWithHeight: %#v", md)
	}
}

func TestBlockNumberUnmarshalInt64AndTmHeight(t *testing.T) {
	tests := []struct {
		input string
		want  BlockNumber
	}{
		{`"earliest"`, EthEarliestBlockNumber},
		{`"latest"`, EthLatestBlockNumber},
		{`"finalized"`, EthLatestBlockNumber},
		{`"safe"`, EthLatestBlockNumber},
		{`"pending"`, EthPendingBlockNumber},
		{`"0xa"`, BlockNumber(10)},
		{`"12"`, BlockNumber(12)},
	}

	for _, tc := range tests {
		var bn BlockNumber
		if err := json.Unmarshal([]byte(tc.input), &bn); err != nil {
			t.Fatalf("unmarshal %s: %v", tc.input, err)
		}
		if bn != tc.want {
			t.Fatalf("unmarshal %s: got %d want %d", tc.input, bn, tc.want)
		}
	}

	for _, input := range []string{`"0xzz"`, `"0x8000000000000000"`} {
		var bn BlockNumber
		if err := json.Unmarshal([]byte(input), &bn); err == nil {
			t.Fatalf("expected error for %s", input)
		}
	}

	if got := EthPendingBlockNumber.Int64(); got != 0 {
		t.Fatalf("pending Int64 mismatch: %d", got)
	}
	if got := EthEarliestBlockNumber.Int64(); got != 1 {
		t.Fatalf("earliest Int64 mismatch: %d", got)
	}
	if got := BlockNumber(5).Int64(); got != 5 {
		t.Fatalf("normal Int64 mismatch: %d", got)
	}

	if h := EthLatestBlockNumber.TmHeight(); h != nil {
		t.Fatalf("latest block should map to nil height, got %v", *h)
	}
	if h := BlockNumber(8).TmHeight(); h == nil || *h != 8 {
		t.Fatalf("block height mismatch: %v", h)
	}
}

func TestBlockNumberOrHashUnmarshalJSON(t *testing.T) {
	var bnh BlockNumberOrHash
	if err := json.Unmarshal([]byte(`{"blockNumber":"0x2a"}`), &bnh); err != nil {
		t.Fatalf("unmarshal object block number: %v", err)
	}
	if bnh.BlockNumber == nil || *bnh.BlockNumber != BlockNumber(42) || bnh.BlockHash != nil {
		t.Fatalf("unexpected blockNumber object result: %#v", bnh)
	}

	hash := common.HexToHash("0x1234000000000000000000000000000000000000000000000000000000000000")
	if err := json.Unmarshal([]byte(`"`+hash.Hex()+`"`), &bnh); err != nil {
		t.Fatalf("unmarshal hash string: %v", err)
	}
	if bnh.BlockHash == nil || *bnh.BlockHash != hash {
		t.Fatalf("unexpected block hash: %#v", bnh.BlockHash)
	}

	if err := json.Unmarshal([]byte(`"pending"`), &bnh); err != nil {
		t.Fatalf("unmarshal pending: %v", err)
	}
	if bnh.BlockNumber == nil || *bnh.BlockNumber != EthPendingBlockNumber {
		t.Fatalf("unexpected pending result: %#v", bnh)
	}

	if err := json.Unmarshal([]byte(`{"blockHash":"`+hash.Hex()+`","blockNumber":"0x1"}`), &bnh); err == nil {
		t.Fatal("expected error when both block hash and block number are set")
	}
	if err := json.Unmarshal([]byte(`"bad"`), &bnh); err == nil {
		t.Fatal("expected error for invalid block string")
	}
}
