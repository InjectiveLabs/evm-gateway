package jsonrpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/rs/cors"
	"golang.org/x/net/netutil"
	"golang.org/x/sync/errgroup"

	"github.com/InjectiveLabs/web3-gateway/internal/config"
	"github.com/InjectiveLabs/web3-gateway/internal/evm/rpc"
	"github.com/InjectiveLabs/web3-gateway/internal/evm/rpc/stream"
	txindexer "github.com/InjectiveLabs/web3-gateway/internal/indexer"
	"github.com/InjectiveLabs/web3-gateway/internal/syncstatus"
	rpcclient "github.com/cometbft/cometbft/rpc/client"
	"github.com/cosmos/cosmos-sdk/client"
	ethlog "github.com/ethereum/go-ethereum/log"
	ethrpc "github.com/ethereum/go-ethereum/rpc"
)

// Start starts the JSON-RPC server
func Start(
	logger *slog.Logger,
	cfg config.Config,
	clientCtx client.Context,
	g *errgroup.Group,
	jsonRPCConfig config.JSONRPCConfig,
	indexer txindexer.TxIndexer,
	status *syncstatus.Tracker,
) (*http.Server, chan struct{}, error) {
	logger = logger.With("module", "jsonrpc")

	evtClient, ok := clientCtx.Client.(rpcclient.EventsClient)
	if !ok {
		return nil, nil, fmt.Errorf("client %T does not implement EventsClient", clientCtx.Client)
	}

	var rpcStreamOpenAttempts = 6
	var rpcStream *stream.RPCStream
	var err error
	for i := 0; i < rpcStreamOpenAttempts; i++ {
		rpcStream, err = stream.NewRPCStreams(evtClient, logger, clientCtx.TxConfig.TxDecoder())
		if err == nil {
			break
		}

		time.Sleep(time.Second)
	}

	if err != nil {
		err = fmt.Errorf("failed to create rpc streams after %d attempts: %w", rpcStreamOpenAttempts, err)
		return nil, nil, err
	}

	handler := NewWrappedSdkLogger(logger)
	ethlog.SetDefault(ethlog.NewLogger(handler))

	rpcServer := ethrpc.NewServer()

	apis := rpc.GetRPCAPIs(
		logger,
		cfg,
		clientCtx,
		rpcStream,
		jsonRPCConfig.AllowUnprotectedTx,
		indexer,
		status,
		jsonRPCConfig.API,
	)

	for _, api := range apis {
		if err := rpcServer.RegisterName(api.Namespace, api.Service); err != nil {
			logger.Error(
				"failed to register service in JSON RPC namespace",
				"namespace", api.Namespace,
				"service", api.Service,
			)
			return nil, nil, err
		}
	}

	r := mux.NewRouter()
	r.HandleFunc("/", rpcServer.ServeHTTP).Methods("POST")
	if status != nil {
		r.HandleFunc("/status/sync", makeSyncStatusHandler(status)).Methods(http.MethodGet)
	}

	handlerWithCors := cors.Default()
	if jsonRPCConfig.EnableUnsafeCors {
		handlerWithCors = cors.AllowAll()
	}

	httpSrv := &http.Server{
		Addr:              jsonRPCConfig.Address,
		Handler:           handlerWithCors.Handler(r),
		ReadHeaderTimeout: jsonRPCConfig.HTTPTimeout,
		ReadTimeout:       jsonRPCConfig.HTTPTimeout,
		WriteTimeout:      jsonRPCConfig.HTTPTimeout,
		IdleTimeout:       jsonRPCConfig.HTTPIdleTimeout,
	}
	httpSrvDone := make(chan struct{}, 1)

	ln, err := listenWithMaxOpenConnections(httpSrv.Addr, jsonRPCConfig.MaxOpenConnections)
	if err != nil {
		return nil, nil, err
	}

	g.Go(func() error {
		logger.Info("Starting JSON-RPC server", "address", jsonRPCConfig.Address)
		if err := httpSrv.Serve(ln); err != nil {
			if errors.Is(err, http.ErrServerClosed) {
				close(httpSrvDone)
			}

			logger.Error("failed to start JSON-RPC server", "error", err.Error())
			return err
		}
		return nil
	})

	if jsonRPCConfig.WsAddress != "" {
		wsSrv := NewWebsocketsServer(clientCtx, logger, rpcStream, jsonRPCConfig)
		wsSrv.Start()
	}
	return httpSrv, httpSrvDone, nil
}

// Listen starts a net.Listener on the tcp network on the given address.
// If there is a specified MaxOpenConnections in the config, it will also set the limitListener.
func listenWithMaxOpenConnections(addr string, maxOpenConnections int) (net.Listener, error) {
	if addr == "" {
		addr = ":http"
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	if maxOpenConnections > 0 {
		ln = netutil.LimitListener(ln, maxOpenConnections)
	}

	return ln, err
}

type WrappedSdkLogger struct {
	logger *slog.Logger
}

func NewWrappedSdkLogger(logger *slog.Logger) *WrappedSdkLogger {
	return &WrappedSdkLogger{
		logger: logger,
	}
}

func (h *WrappedSdkLogger) Handle(ctx context.Context, r slog.Record) error {
	switch r.Level {
	case ethlog.LvlTrace, ethlog.LvlDebug:
		h.logger.Debug(r.Message)
	case ethlog.LvlInfo, ethlog.LevelWarn:
		h.logger.Info(r.Message)
	case ethlog.LevelError, ethlog.LevelCrit:
		h.logger.Error(r.Message)
	}
	return nil
}

func (h *WrappedSdkLogger) Enabled(_ context.Context, level slog.Level) bool {
	return true
}

func (h *WrappedSdkLogger) WithGroup(_ string) slog.Handler {
	return h
}

func (h *WrappedSdkLogger) WithAttrs(_ []slog.Attr) slog.Handler {
	return h
}
