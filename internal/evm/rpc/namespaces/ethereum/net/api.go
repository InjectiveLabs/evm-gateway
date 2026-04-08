package net

import (
	"context"
	"fmt"

	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
	cmrpcclient "github.com/cometbft/cometbft/rpc/client"
	"github.com/cosmos/cosmos-sdk/client"
	"upd.dev/xlab/gotracer"
)

var netTraceTag = gotracer.NewTag("component", "eth_net")

// PublicAPI is the eth_ prefixed set of APIs in the Web3 JSON-RPC spec.
type PublicAPI struct {
	networkVersion uint64
	tmRPCClient    cmrpcclient.Client
}

// NewPublicAPI creates an instance of the public Net Web3 API.
func NewPublicAPI(clientCtx client.Context) *PublicAPI {
	// parse the chainID from a integer string
	chainIDEpoch, err := chaintypes.ParseChainID(clientCtx.ChainID)
	if err != nil {
		panic(err)
	}

	api := &PublicAPI{networkVersion: chainIDEpoch.Uint64()}
	if client, ok := clientCtx.Client.(cmrpcclient.Client); ok {
		api.tmRPCClient = client
	}
	return api
}

// Version returns the current ethereum protocol version.
func (s *PublicAPI) Version() string {
	return fmt.Sprintf("%d", s.networkVersion)
}

// Listening returns if client is actively listening for network connections.
func (s *PublicAPI) Listening() bool {
	ctx := context.Background()
	defer gotracer.Traceless(&ctx, netTraceTag)()

	if s.tmRPCClient == nil {
		return false
	}
	netInfo, err := s.tmRPCClient.NetInfo(ctx)
	if err != nil {
		return false
	}
	return netInfo.Listening
}

// PeerCount returns the number of peers currently connected to the client.
func (s *PublicAPI) PeerCount() int {
	ctx := context.Background()
	defer gotracer.Traceless(&ctx, netTraceTag)()

	if s.tmRPCClient == nil {
		return 0
	}
	netInfo, err := s.tmRPCClient.NetInfo(ctx)
	if err != nil {
		return 0
	}
	return len(netInfo.Peers)
}
