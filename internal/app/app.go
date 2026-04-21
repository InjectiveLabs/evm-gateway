package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"syscall"
	"time"

	"upd.dev/xlab/gotracer"

	cmrpcclient "github.com/cometbft/cometbft/rpc/client"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	cmtrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
	chainclient "github.com/InjectiveLabs/sdk-go/client/chain"

	"github.com/InjectiveLabs/evm-gateway/internal/blocksync"
	"github.com/InjectiveLabs/evm-gateway/internal/config"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	txindexer "github.com/InjectiveLabs/evm-gateway/internal/indexer"
	"github.com/InjectiveLabs/evm-gateway/internal/jsonrpc"
	"github.com/InjectiveLabs/evm-gateway/internal/syncstatus"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

var appTraceTag = gotracer.NewTag("component", "app")

const cpuProfileEnvVar = "WEB3INJ_DEBUG_CPU_PROFILE_PATH"

// Run starts the evm-gateway services and blocks until shutdown.
func Run(cfg config.Config, logger *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	dataDir, err := expandHome(cfg.DataDir)
	if err != nil {
		return err
	}
	stopCPUProfile, err := startCPUProfileFromEnv(logger)
	if err != nil {
		return err
	}
	defer stopCPUProfile()
	defer func() {
		if err := blocksync.CloseFetchTimingRecorder(); err != nil {
			logger.Warn("failed to close blocksync timing recorder", "error", err)
		}
	}()

	clientCtx, rpcClient, grpcConn, err := buildClientContext(ctx, &cfg, dataDir, logger)
	if err != nil {
		return err
	}
	defer func() {
		if rpcClient != nil {
			_ = rpcClient.Stop()
		}
		if grpcConn != nil {
			_ = grpcConn.Close()
		}
	}()

	var idxDB dbm.DB
	var txIndexer txindexer.TxIndexer
	var statusTracker *syncstatus.Tracker
	if cfg.JSONRPC.Enable || cfg.EnableSync {
		idxDB, err = openIndexerDB(dataDir, cfg.DBBackend)
		if err != nil {
			return err
		}
		defer func() {
			if idxDB != nil {
				_ = idxDB.Close()
			}
		}()
		idxLogger := logger.With("indexer", "evm")
		txIndexer = txindexer.NewKVIndexer(idxDB, idxLogger, clientCtx, buildKVIndexerOptions(ctx, cfg, clientCtx, logger)...)
		statusTracker = syncstatus.NewTracker(cfg.FetchJobs, cfg.Earliest)
	}

	g, gctx := errgroup.WithContext(ctx)

	if txIndexer != nil && cfg.EnableSync {
		syncer := txindexer.NewSyncer(cfg, logger, rpcClient, idxDB, txIndexer, statusTracker)
		g.Go(func() error {
			return syncer.Run(gctx)
		})
	}

	var httpSrv *http.Server
	var httpSrvDone chan struct{}
	if cfg.JSONRPC.Enable {
		var err error
		httpSrv, httpSrvDone, err = jsonrpc.Start(logger, cfg, clientCtx, g, cfg.JSONRPC, txIndexer, statusTracker)
		if err != nil {
			return err
		}
	}

	shutdownCh := make(chan struct{})
	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout)
			defer shutdownCancel()

			if httpSrv != nil {
				_ = httpSrv.Shutdown(shutdownCtx)
				if httpSrvDone != nil {
					select {
					case <-httpSrvDone:
					case <-shutdownCtx.Done():
					}
				}
			}

			close(shutdownCh)
		})
	}

	go func() {
		<-gctx.Done()
		shutdown()
	}()

	err = g.Wait()
	shutdown()

	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	logger.Info("shutdown complete", "after", cfg.Shutdown.Timeout)
	return nil
}

// RunResync reindexes the requested block ranges and exits.
func RunResync(cfg config.Config, logger *slog.Logger, targets []txindexer.BlockRange) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	defer gotracer.Trace(&ctx, appTraceTag)()

	dataDir, err := expandHome(cfg.DataDir)
	if err != nil {
		return err
	}
	stopCPUProfile, err := startCPUProfileFromEnv(logger)
	if err != nil {
		return err
	}
	defer stopCPUProfile()
	defer func() {
		if err := blocksync.CloseFetchTimingRecorder(); err != nil {
			logger.Warn("failed to close blocksync timing recorder", "error", err)
		}
	}()

	clientCtx, rpcClient, grpcConn, err := buildClientContext(ctx, &cfg, dataDir, logger)
	if err != nil {
		return err
	}
	defer func() {
		if rpcClient != nil {
			_ = rpcClient.Stop()
		}
		if grpcConn != nil {
			_ = grpcConn.Close()
		}
	}()

	idxDB, err := openIndexerDB(dataDir, cfg.DBBackend)
	if err != nil {
		return err
	}
	defer func() {
		_ = idxDB.Close()
	}()

	idxLogger := logger.With("indexer", "evm")
	txIndexer := txindexer.NewKVIndexer(idxDB, idxLogger, clientCtx, buildKVIndexerOptions(ctx, cfg, clientCtx, logger)...)
	syncer := txindexer.NewSyncer(cfg, logger, rpcClient, idxDB, txIndexer, nil)

	startedAt := time.Now()
	stats, err := syncer.Resync(ctx, targets)
	if err != nil {
		return err
	}

	logger.Info(
		"resync complete",
		"time_passed", time.Since(startedAt),
		"blocks_synced", stats.BlocksSynced,
		"unique_txns_seen", stats.UniqueTxnsSeen,
	)
	return nil
}

type cometStatusClient interface {
	Status(context.Context) (*cmtrpctypes.ResultStatus, error)
}

type evmParamsClient interface {
	Params(context.Context, *evmtypes.QueryParamsRequest, ...grpc.CallOption) (*evmtypes.QueryParamsResponse, error)
}

func buildKVIndexerOptions(ctx context.Context, cfg config.Config, clientCtx client.Context, logger *slog.Logger) []txindexer.KVIndexerOption {
	opts := []txindexer.KVIndexerOption{
		txindexer.WithVirtualBankTransfers(cfg.VirtualizeCosmosEvents, cfg.EVMChainID),
	}

	gasLimit, ok, err := fetchStartupBlockGasLimit(ctx, clientCtx)
	if err != nil {
		logger.Warn("failed to cache startup block gas limit", "error", err)
		return opts
	}
	if !ok {
		return opts
	}
	logger.Info("cached startup block gas limit", "gas_limit", gasLimit)
	return append(opts, txindexer.WithCachedBlockGasLimit(gasLimit))
}

func fetchStartupBlockGasLimit(ctx context.Context, clientCtx client.Context) (uint64, bool, error) {
	if clientCtx.Client == nil {
		return 0, false, nil
	}

	statusClient, ok := clientCtx.Client.(cometStatusClient)
	if !ok {
		return 0, false, nil
	}
	if _, ok := clientCtx.Client.(cmrpcclient.Client); !ok {
		return 0, false, nil
	}

	status, err := statusClient.Status(ctx)
	if err != nil {
		return 0, false, errors.Wrap(err, "fetch latest block height")
	}
	height := status.SyncInfo.LatestBlockHeight
	if height <= 0 {
		return 0, false, nil
	}

	gasLimit, err := rpctypes.BlockMaxGasFromConsensusParams(ctx, clientCtx, height)
	if err != nil {
		return 0, false, errors.Wrap(err, "fetch consensus params")
	}
	if gasLimit < 0 {
		return 0, false, nil
	}

	return uint64(gasLimit), true, nil
}

func startCPUProfileFromEnv(logger *slog.Logger) (func(), error) {
	path := os.Getenv(cpuProfileEnvVar)
	if path == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, errors.Wrap(err, "create cpu profile directory")
	}
	file, err := os.Create(path)
	if err != nil {
		return nil, errors.Wrap(err, "create cpu profile file")
	}
	if err := pprof.StartCPUProfile(file); err != nil {
		_ = file.Close()
		return nil, errors.Wrap(err, "start cpu profile")
	}
	logger.Info("cpu profiling enabled", "path", path)
	return func() {
		pprof.StopCPUProfile()
		_ = file.Close()
	}, nil
}

func buildClientContext(ctx context.Context, cfg *config.Config, dataDir string, logger *slog.Logger) (client.Context, *rpchttp.HTTP, *grpc.ClientConn, error) {
	defer gotracer.Trace(&ctx, appTraceTag)()

	clientCtx, err := baseClientContext(ctx, dataDir)
	if err != nil {
		return client.Context{}, nil, nil, errors.Wrap(err, "init injective client context")
	}

	if cfg.OfflineRPCOnly {
		if strings.TrimSpace(cfg.EVMChainID) == "" {
			evmChainID, err := inferEVMChainIDFromCosmosChainID(cfg.ChainID)
			if err != nil {
				return client.Context{}, nil, nil, err
			}
			cfg.EVMChainID = evmChainID
			logger.Warn(
				"offline rpc-only mode falling back to evm chain id derived from cosmos chain id; set evm-chain-id to override",
				"chain_id", cfg.ChainID,
				"evm_chain_id", cfg.EVMChainID,
			)
		}
		logger.Warn(
			"starting in offline rpc-only mode; live comet/grpc clients are disabled",
			"chain_id", cfg.ChainID,
			"evm_chain_id", cfg.EVMChainID,
			"enable_sync", cfg.EnableSync,
		)
		clientCtx = clientCtx.WithChainID(cfg.ChainID)
		return clientCtx, nil, nil, nil
	}

	clientCtx = clientCtx.WithNodeURI(cfg.CometRPC)

	rpcClient, err := rpchttp.NewWithTimeout(cfg.CometRPC, 10)
	if err != nil {
		return client.Context{}, nil, nil, errors.Wrap(err, "init comet rpc client")
	}
	clientCtx = clientCtx.WithClient(rpcClient)

	chainID, err := cometChainID(ctx, rpcClient)
	if err != nil {
		return client.Context{}, nil, nil, err
	}
	if err := validateCometChainID(cfg, chainID); err != nil {
		return client.Context{}, nil, nil, err
	}
	clientCtx = clientCtx.WithChainID(cfg.ChainID)

	grpcConn, err := dialGRPC(ctx, cfg.GRPCAddr, clientCtx.InterfaceRegistry)
	if err != nil {
		return client.Context{}, nil, nil, err
	}
	clientCtx = clientCtx.WithGRPCClient(grpcConn)
	logger.Info("grpc client ready", "address", cfg.GRPCAddr)

	evmChainID, err := fetchEVMChainID(ctx, rpctypes.NewQueryClient(clientCtx))
	if err != nil {
		_ = grpcConn.Close()
		return client.Context{}, nil, nil, err
	}
	if err := validateEVMChainID(cfg, evmChainID); err != nil {
		_ = grpcConn.Close()
		return client.Context{}, nil, nil, err
	}
	logger.Info("resolved startup chain ids", "chain_id", cfg.ChainID, "evm_chain_id", cfg.EVMChainID)

	return clientCtx, rpcClient, grpcConn, nil
}

func baseClientContext(ctx context.Context, dataDir string) (client.Context, error) {
	clientCtx, err := chainclient.NewClientContext("", "", nil)
	if err != nil {
		return client.Context{}, err
	}
	return clientCtx.
		WithCmdContext(ctx).
		WithBroadcastMode(flags.BroadcastSync).
		WithHomeDir(dataDir), nil
}

func cometChainID(ctx context.Context, rpcClient cometStatusClient) (string, error) {
	defer gotracer.Trace(&ctx, appTraceTag)()

	status, err := rpcClient.Status(ctx)
	if err != nil {
		return "", errors.Wrap(err, "fetch node status")
	}

	chainID := strings.TrimSpace(status.NodeInfo.Network)
	if chainID == "" {
		return "", errors.New("empty chain id from comet status")
	}

	return chainID, nil
}

func validateCometChainID(cfg *config.Config, cometChainID string) error {
	if cfg.ChainID != "" && cfg.ChainID != cometChainID {
		return fmt.Errorf("chain id mismatch: expected %s, got %s", cfg.ChainID, cometChainID)
	}

	cfg.ChainID = cometChainID
	return nil
}

func fetchEVMChainID(ctx context.Context, queryClient evmParamsClient) (string, error) {
	defer gotracer.Trace(&ctx, appTraceTag)()

	res, err := queryClient.Params(ctx, &evmtypes.QueryParamsRequest{})
	if err != nil {
		return "", errors.Wrap(err, "fetch evm params")
	}
	if res == nil || res.Params.ChainConfig.EIP155ChainID == nil {
		return "", errors.New("empty evm chain id from params")
	}

	chainID := strings.TrimSpace(res.Params.ChainConfig.EIP155ChainID.String())
	if chainID == "" || chainID == "0" {
		return "", errors.New("empty evm chain id from params")
	}

	return chainID, nil
}

func validateEVMChainID(cfg *config.Config, evmChainID string) error {
	evmChainID = strings.TrimSpace(evmChainID)
	if cfg.EVMChainID != "" && cfg.EVMChainID != evmChainID {
		return fmt.Errorf("evm chain id mismatch: expected %s, got %s", cfg.EVMChainID, evmChainID)
	}

	cfg.EVMChainID = evmChainID
	return nil
}

func inferEVMChainIDFromCosmosChainID(chainID string) (string, error) {
	parsed, err := chaintypes.ParseChainID(strings.TrimSpace(chainID))
	if err != nil {
		return "", errors.Wrap(err, "derive evm chain id from cosmos chain id")
	}
	return parsed.String(), nil
}

func dialGRPC(ctx context.Context, addr string, registry codectypes.InterfaceRegistry) (*grpc.ClientConn, error) {
	defer gotracer.Trace(&ctx, appTraceTag)()

	dialTarget := normalizedGRPCTarget(addr)
	transportCreds := grpcTransportCredentials(addr)

	return grpc.DialContext(
		ctx,
		dialTarget,
		grpc.WithTransportCredentials(transportCreds),
		grpc.WithContextDialer(dialer),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(codec.NewProtoCodec(registry).GRPCCodec())),
	)
}

func grpcTransportCredentials(addr string) credentials.TransportCredentials {
	proto, address := protocolAndAddress(addr)
	switch proto {
	case "https", "grpcs":
		serverName := hostWithoutPort(address)
		if host, _, err := net.SplitHostPort(address); err == nil {
			serverName = host
		}
		return credentials.NewTLS(&tls.Config{ServerName: serverName})
	default:
		return insecure.NewCredentials()
	}
}

func normalizedGRPCTarget(addr string) string {
	_, address := protocolAndAddress(addr)
	return address
}

func dialer(_ context.Context, addr string) (net.Conn, error) {
	proto, address := protocolAndAddress(addr)
	return net.Dial(proto, address)
}

func protocolAndAddress(listenAddr string) (string, string) {
	protocol, address := "tcp", listenAddr
	parts := strings.SplitN(address, "://", 2)
	if len(parts) == 2 {
		protocol, address = parts[0], parts[1]
	}
	address = withDefaultPort(protocol, address)
	return protocol, address
}

func withDefaultPort(protocol, address string) string {
	if address == "" {
		return address
	}
	if _, _, err := net.SplitHostPort(address); err == nil {
		return address
	}
	host := hostWithoutPort(address)
	if host == "" {
		return address
	}

	switch protocol {
	case "https", "grpcs":
		return net.JoinHostPort(host, "443")
	case "http":
		return net.JoinHostPort(host, "80")
	default:
		return address
	}
}

func hostWithoutPort(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
		return strings.TrimSuffix(strings.TrimPrefix(address, "["), "]")
	}
	if parsed, err := url.Parse("scheme://" + address); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	return address
}

func openIndexerDB(rootDir string, backend string) (dbm.DB, error) {
	dataDir := filepath.Join(rootDir, "data")
	return dbm.NewDB("evmindexer", dbm.BackendType(backend), dataDir)
}

func expandHome(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}
	if path == "~" {
		return os.UserHomeDir()
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}
