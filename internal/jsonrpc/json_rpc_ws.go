package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"sync"
	"time"

	rpcfilters "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/namespaces/ethereum/eth/filters"
	rpcstream "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/stream"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtual"
	"github.com/bytedance/sonic"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
	"upd.dev/xlab/gotracer"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
)

const (
	messageSizeLimit             = 10 * 1024 * 1024 // 10MB
	websocketPingInterval        = 20 * time.Second
	websocketControlWriteTimeout = 5 * time.Second
)

type WebsocketsServer interface {
	Start()
}

type SubscriptionResponseJSON struct {
	Jsonrpc string          `json:"jsonrpc"`
	Result  interface{}     `json:"result"`
	ID      json.RawMessage `json:"id"`
}

type SubscriptionNotification struct {
	Jsonrpc string              `json:"jsonrpc"`
	Method  string              `json:"method"`
	Params  *SubscriptionResult `json:"params"`
}

type SubscriptionResult struct {
	Subscription rpc.ID               `json:"subscription"`
	Result       interface{}          `json:"result"`
	Metadata     SubscriptionMetadata `json:"metadata"`
}

type SubscriptionMetadata struct {
	EmittedAtUnixNano int64 `json:"emittedAtUnixNano"`
}

type ErrorResponseJSON struct {
	Jsonrpc string            `json:"jsonrpc"`
	Error   *ErrorMessageJSON `json:"error"`
	ID      *big.Int          `json:"id"`
}

type ErrorMessageJSON struct {
	Code    *big.Int `json:"code"`
	Message string   `json:"message"`
}

type wsRPCRequest struct {
	Method string          `json:"method"`
	Params []interface{}   `json:"params"`
	ID     json.RawMessage `json:"id"`
}

// subscriber opens eth_subscribe subscriptions. pubSubAPI is the production
// implementation; tests substitute it to observe subscription lifecycles.
type subscriber interface {
	subscribe(wsConn *wsConn, subID rpc.ID, params []interface{}) (context.CancelFunc, error)
}

type websocketsServer struct {
	rpcAddr      string // listen address of rest-server
	wsAddr       string // listen address of ws server
	api          subscriber
	logger       *slog.Logger
	pingInterval time.Duration
}

func NewWebsocketsServer(
	clientCtx client.Context,
	logger *slog.Logger,
	stream *rpcstream.RPCStream,
	jsonRPCConfig config.JSONRPCConfig,
) WebsocketsServer {
	logger = logger.With("api", "websocket-server")
	_, port, _ := net.SplitHostPort(jsonRPCConfig.Address)

	logger.Info("Starting JSON WebSocket server", "address", jsonRPCConfig.WsAddress)

	return &websocketsServer{
		rpcAddr:      "localhost:" + port, // FIXME: this shouldn't be hardcoded to localhost
		wsAddr:       jsonRPCConfig.WsAddress,
		api:          newPubSubAPI(clientCtx, logger, stream),
		logger:       logger,
		pingInterval: websocketPingInterval,
	}
}

func (s *websocketsServer) Start() {
	ws := mux.NewRouter()
	ws.Handle("/", s)

	go func() {
		err := http.ListenAndServe(s.wsAddr, ws) // #nosec G114 -- http functions have no support for timeouts
		if err != nil {
			if err == http.ErrServerClosed {
				return
			}

			s.logger.Error("failed to start HTTP server for WS", "error", err.Error())
		}
	}()
}

func (s *websocketsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(_ *http.Request) bool {
			return true
		},
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Debug("websocket upgrade failed", "error", err.Error())
		return
	}
	conn.SetReadLimit(messageSizeLimit)

	peer := &wsConn{
		mux:  new(sync.Mutex),
		conn: conn,
	}
	interval := s.pingInterval
	if interval <= 0 {
		interval = websocketPingInterval
	}
	ctx, cancel := context.WithCancel(r.Context())
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		peer.heartbeat(ctx, interval)
	}()
	defer func() {
		cancel()
		_ = peer.Close()
		<-heartbeatDone
	}()

	s.readLoop(peer)
}

func (s *websocketsServer) sendErrResponse(wsConn *wsConn, msg string) error {
	res := &ErrorResponseJSON{
		Jsonrpc: "2.0",
		Error: &ErrorMessageJSON{
			Code:    big.NewInt(-32600),
			Message: msg,
		},
		ID: nil,
	}

	return wsConn.WriteJSON(res)
}

type wsConn struct {
	conn *websocket.Conn
	mux  *sync.Mutex
}

func (w *wsConn) WriteJSON(v interface{}) error {
	bz, err := sonic.Marshal(v)
	if err != nil {
		return err
	}
	return w.WriteMessage(websocket.TextMessage, bz)
}

// WriteMessage serializes websocket writes so concurrent subscription senders
// cannot interleave frames on the same connection.
func (w *wsConn) WriteMessage(messageType int, data []byte) error {
	w.mux.Lock()
	defer w.mux.Unlock()

	return w.conn.WriteMessage(messageType, data)
}

func (w *wsConn) Close() error {
	// Gorilla permits Close concurrently with writes. Do not wait for a stalled
	// subscription writer before closing the socket and unblocking it.
	return w.conn.Close()
}

func (w *wsConn) heartbeat(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Application traffic already keeps the connection active. Do not let
			// a heartbeat deadline interrupt a slow archival response.
			if !w.mux.TryLock() {
				continue
			}
			err := w.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(websocketControlWriteTimeout))
			w.mux.Unlock()
			if err != nil {
				_ = w.Close()
				return
			}
		}
	}
}

func (w *wsConn) ReadMessage() (messageType int, p []byte, err error) {
	// not protected by write mutex

	return w.conn.ReadMessage()
}

func (s *websocketsServer) readLoop(wsConn *wsConn) {
	// subscriptions of current connection
	subscriptions := make(map[rpc.ID]context.CancelFunc)
	defer func() {
		// cancel all subscriptions when connection closed
		for _, unsubFn := range subscriptions {
			unsubFn()
		}
	}()

	for {
		_, mb, err := wsConn.ReadMessage()
		if err != nil {
			_ = wsConn.Close()
			if isExpectedWebsocketClose(err) {
				s.logger.Debug("websocket peer closed", "error", err.Error())
			} else {
				s.logger.Error("read message error, breaking read loop", "error", err.Error())
			}
			return
		}

		if err := s.handleMessage(wsConn, mb, subscriptions); err != nil {
			// The peer cannot receive responses: gorilla/websocket keeps a write
			// error sticky, so every later response would fail too. Stop reading
			// so that requests already buffered on the socket are not served
			// (no new subscriptions, no proxied RPC work) for a dead peer.
			// Returning runs the deferred cleanup that cancels all subscriptions.
			_ = wsConn.Close()
			s.logger.Debug("failed to write websocket response, closing peer", "error", err.Error())
			return
		}
	}
}

// handleMessage serves a single websocket request. It returns an error only
// when the response could not be written to the peer; request-level failures
// are reported to the peer as JSON-RPC errors.
func (s *websocketsServer) handleMessage(wsConn *wsConn, mb []byte, subscriptions map[rpc.ID]context.CancelFunc) error {
	if isBatch(mb) {
		return s.forwardRequest(wsConn, mb)
	}

	var msg wsRPCRequest
	if err := sonic.Unmarshal(mb, &msg); err != nil {
		return s.sendErrResponse(wsConn, err.Error())
	}

	switch msg.Method {
	case "eth_subscribe":
		if err := checkParams(msg.Params); err != nil {
			return s.sendErrResponse(wsConn, err.Error())
		}

		subID := rpc.NewID()
		unsubFn, err := s.api.subscribe(wsConn, subID, msg.Params)
		if err != nil {
			return s.sendErrResponse(wsConn, err.Error())
		}
		subscriptions[subID] = unsubFn

		return wsConn.WriteJSON(&SubscriptionResponseJSON{
			Jsonrpc: "2.0",
			ID:      msg.ID,
			Result:  subID,
		})
	case "eth_unsubscribe":
		if err := checkParams(msg.Params); err != nil {
			return s.sendErrResponse(wsConn, err.Error())
		}

		id, ok := msg.Params[0].(string)
		if !ok {
			return s.sendErrResponse(wsConn, "invalid parameters")
		}

		subID := rpc.ID(id)
		unsubFn, ok := subscriptions[subID]
		if ok {
			delete(subscriptions, subID)
			unsubFn()
		}

		return wsConn.WriteJSON(&SubscriptionResponseJSON{
			Jsonrpc: "2.0",
			ID:      msg.ID,
			Result:  ok,
		})
	default:
		// any other method (or a request without one) goes to the usual rpc server
		return s.forwardRequest(wsConn, mb)
	}
}

func isExpectedWebsocketClose(err error) bool {
	return websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) ||
		errors.Is(err, net.ErrClosed)
}

// checkParams validates the params of eth_subscribe and eth_unsubscribe.
func checkParams(params []interface{}) error {
	if params == nil {
		return errors.New("invalid parameters")
	}

	if len(params) == 0 {
		return errors.New("empty parameters")
	}

	return nil
}

// forwardRequest proxies a request to the rest-server and writes its response
// to the peer. A failed proxy call is reported to the peer as a JSON-RPC error.
// The returned error is non-nil only if the peer could not be written to.
func (s *websocketsServer) forwardRequest(wsConn *wsConn, mb []byte) error {
	body, err := s.tcpGetResponse(mb)
	if err != nil {
		return s.sendErrResponse(wsConn, err.Error())
	}

	return wsConn.WriteMessage(websocket.TextMessage, body)
}

// tcpGetResponse connects to the rest-server over tcp, posts a JSON-RPC request, and returns the response body.
func (s *websocketsServer) tcpGetResponse(mb []byte) ([]byte, error) {
	ctx := context.Background()
	defer gotracer.Traceless(&ctx, jsonRPCTraceTag)()

	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+s.rpcAddr, bytes.NewBuffer(mb))
	if err != nil {
		return nil, errors.Wrap(err, "Could not build request")
	}

	req.Header.Set("Content-Type", "application/json")
	httpClient := &http.Client{}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "Could not perform request")
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "could not read body from response")
	}

	return body, nil
}

// pubSubAPI is the eth_ prefixed set of APIs in the Web3 JSON-RPC spec
type pubSubAPI struct {
	events    *rpcstream.RPCStream
	logger    *slog.Logger
	clientCtx client.Context
}

// newPubSubAPI creates an instance of the ethereum PubSub API.
func newPubSubAPI(clientCtx client.Context, logger *slog.Logger, stream *rpcstream.RPCStream) *pubSubAPI {
	logger = logger.With("module", "ws-client")
	return &pubSubAPI{
		events:    stream,
		logger:    logger,
		clientCtx: clientCtx,
	}
}

func (api *pubSubAPI) subscribe(wsConn *wsConn, subID rpc.ID, params []interface{}) (context.CancelFunc, error) {
	if api.events == nil {
		return nil, errors.New("eth_subscribe is unavailable in polling-only mode")
	}

	method, ok := params[0].(string)
	if !ok {
		return nil, errors.New("invalid parameters")
	}

	switch method {
	case "newHeads":
		// TODO: handle extra params
		return api.subscribeNewHeads(wsConn, subID)
	case "logs":
		if len(params) > 1 {
			return api.subscribeLogs(wsConn, subID, params[1])
		}
		return api.subscribeLogs(wsConn, subID, nil)
	case "newPendingTransactions":
		return api.subscribePendingTransactions(wsConn, subID)
	case "syncing":
		return api.subscribeSyncing(wsConn, subID)
	default:
		return nil, errors.Errorf("unsupported method %s", method)
	}
}

func (api *pubSubAPI) subscribeNewHeads(wsConn *wsConn, subID rpc.ID) (context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	//nolint:errcheck // for one liner
	go api.events.HeaderStream().Subscribe(ctx, func(headers []rpcstream.RPCHeader, _ int) error {
		for _, header := range headers {
			if err := api.writeSubscriptionResult(wsConn, subID, header.EthHeader); err != nil {
				return err
			}
		}
		return nil
	})

	return cancel, nil
}

func (api *pubSubAPI) writeSubscriptionResult(wsConn *wsConn, subID rpc.ID, result interface{}) error {
	res := &SubscriptionNotification{
		Jsonrpc: "2.0",
		Method:  "eth_subscription",
		Params: &SubscriptionResult{
			Subscription: subID,
			Result:       result,
			Metadata: SubscriptionMetadata{
				EmittedAtUnixNano: time.Now().UnixNano(),
			},
		},
	}

	if err := wsConn.WriteJSON(res); err != nil {
		api.logger.Debug("subscription write failed; closing peer", "error", err.Error())
		try(func() {
			if !errors.Is(err, websocket.ErrCloseSent) {
				_ = wsConn.Close()
			}
		}, api.logger, "closing websocket peer sub")
		return err
	}
	return nil
}

func try(fn func(), l *slog.Logger, desc string) {
	defer func() {
		if x := recover(); x != nil {
			if err, ok := x.(error); ok {
				l.Debug("panic during "+desc, "error", err.Error())
				return
			}

			l.Debug(fmt.Sprintf("panic during %s: %+v", desc, x))
			return
		}
	}()

	fn()
}

func (api *pubSubAPI) subscribeLogs(wsConn *wsConn, subID rpc.ID, extra interface{}) (context.CancelFunc, error) {
	crit := filters.FilterCriteria{}

	if extra != nil {
		params, ok := extra.(map[string]interface{})
		if !ok {
			err := errors.New("invalid criteria")
			api.logger.Debug("invalid criteria", "type", fmt.Sprintf("%T", extra))
			return nil, err
		}

		if params["address"] != nil {
			switch address := params["address"].(type) {
			case string:
				crit.Addresses = []common.Address{common.HexToAddress(address)}
			case []interface{}:
				for _, addr := range address {
					address, ok := addr.(string)
					if !ok {
						return nil, errors.New("invalid address")
					}

					crit.Addresses = append(crit.Addresses, common.HexToAddress(address))
				}
			default:
				return nil, errors.New("invalid addresses; must be address or array of addresses")
			}
		}

		if params["topics"] != nil {
			topics, ok := params["topics"].([]interface{})
			if !ok {
				err := errors.Errorf("invalid topics: %s", topics)
				api.logger.Error("invalid topics", "type", fmt.Sprintf("%T", topics))
				return nil, err
			}

			crit.Topics = make([][]common.Hash, len(topics))

			addCritTopic := func(topicIdx int, topic interface{}) error {
				tstr, ok := topic.(string)
				if !ok {
					err := errors.Errorf("invalid topic: %s", topic)
					api.logger.Error("invalid topic", "type", fmt.Sprintf("%T", topic))
					return err
				}

				crit.Topics[topicIdx] = []common.Hash{common.HexToHash(tstr)}
				return nil
			}

			for topicIdx, subtopics := range topics {
				if subtopics == nil {
					continue
				}

				// in case we don't have list, but a single topic value
				if topic, ok := subtopics.(string); ok {
					if err := addCritTopic(topicIdx, topic); err != nil {
						return nil, err
					}

					continue
				}

				// in case we actually have a list of subtopics
				subtopicsList, ok := subtopics.([]interface{})
				if !ok {
					err := errors.New("invalid subtopics")
					api.logger.Error("invalid subtopic", "type", fmt.Sprintf("%T", subtopics))
					return nil, err
				}

				subtopicsCollect := make([]common.Hash, len(subtopicsList))
				for idx, subtopic := range subtopicsList {
					tstr, ok := subtopic.(string)
					if !ok {
						err := errors.Errorf("invalid subtopic: %s", subtopic)
						api.logger.Error("invalid subtopic", "type", fmt.Sprintf("%T", subtopic))
						return nil, err
					}

					subtopicsCollect[idx] = common.HexToHash(tstr)
				}

				crit.Topics[topicIdx] = subtopicsCollect
			}
		}
	}

	if err := rpcfilters.ValidateFilterCriteria(crit); err != nil {
		return nil, errors.Wrap(err, "invalid filter criteria")
	}

	ctx, cancel := context.WithCancel(context.Background())
	//nolint:errcheck // one liner
	go api.events.LogStream().Subscribe(ctx, func(txLogs []*virtual.RPCLog, _ int) error {
		logs := rpcfilters.FilterLogs(txLogs, crit.FromBlock, crit.ToBlock, crit.Addresses, crit.Topics)
		if len(logs) == 0 {
			return nil
		}

		for _, ethLog := range logs {
			if err := api.writeSubscriptionResult(wsConn, subID, ethLog); err != nil {
				return err
			}
		}
		return nil
	})

	return cancel, nil
}

func (api *pubSubAPI) subscribePendingTransactions(wsConn *wsConn, subID rpc.ID) (context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	//nolint:errcheck // one liner
	go api.events.PendingTxStream().Subscribe(ctx, func(items []common.Hash, _ int) error {
		for _, hash := range items {
			if err := api.writeSubscriptionResult(wsConn, subID, hash); err != nil {
				return err
			}
		}
		return nil
	})

	return cancel, nil
}

func (api *pubSubAPI) subscribeSyncing(_ *wsConn, _ rpc.ID) (context.CancelFunc, error) {
	return nil, errors.New("syncing subscription is not implemented")
}

// copy from github.com/ethereum/go-ethereum/rpc/json.go
// isBatch returns true when the first non-whitespace characters is '['
func isBatch(raw []byte) bool {
	for _, c := range raw {
		// skip insignificant whitespace (http://www.ietf.org/rfc/rfc4627.txt)
		if c == 0x20 || c == 0x09 || c == 0x0a || c == 0x0d {
			continue
		}
		return c == '['
	}
	return false
}
