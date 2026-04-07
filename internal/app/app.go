package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"upd.dev/xlab/gotracer"

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

	chainclient "github.com/InjectiveLabs/sdk-go/client/chain"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
	txindexer "github.com/InjectiveLabs/evm-gateway/internal/indexer"
	"github.com/InjectiveLabs/evm-gateway/internal/jsonrpc"
	"github.com/InjectiveLabs/evm-gateway/internal/syncstatus"
)

var appTraceTag = gotracer.NewTag("component", "app")

// Run starts the evm-gateway services and blocks until shutdown.
func Run(cfg config.Config, logger *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	dataDir, err := expandHome(cfg.DataDir)
	if err != nil {
		return err
	}

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
		txIndexer = txindexer.NewKVIndexer(idxDB, idxLogger, clientCtx)
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
	txIndexer := txindexer.NewKVIndexer(idxDB, idxLogger, clientCtx)
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

func buildClientContext(ctx context.Context, cfg *config.Config, dataDir string, logger *slog.Logger) (client.Context, *rpchttp.HTTP, *grpc.ClientConn, error) {
	defer gotracer.Trace(&ctx, appTraceTag)()

	clientCtx, err := baseClientContext(ctx, dataDir)
	if err != nil {
		return client.Context{}, nil, nil, errors.Wrap(err, "init injective client context")
	}

	if cfg.OfflineRPCOnly {
		logger.Warn(
			"starting in offline rpc-only mode; live comet/grpc clients are disabled",
			"chain_id", cfg.ChainID,
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
		serverName := address
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
	return protocol, address
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
