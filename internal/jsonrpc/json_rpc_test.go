package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/pkg/errors"
)

type fakeEventClient struct {
	running    bool
	startErr   error
	startCalls int
}

func (c *fakeEventClient) Start() error {
	c.startCalls++
	if c.startErr != nil {
		return c.startErr
	}

	c.running = true
	return nil
}

func (c *fakeEventClient) IsRunning() bool {
	return c.running
}

func (c *fakeEventClient) Subscribe(context.Context, string, string, ...int) (<-chan coretypes.ResultEvent, error) {
	return nil, nil
}

func (c *fakeEventClient) Unsubscribe(context.Context, string, string) error {
	return nil
}

func (c *fakeEventClient) UnsubscribeAll(context.Context, string) error {
	return nil
}

type passiveEventClient struct{}

func (c *passiveEventClient) Subscribe(context.Context, string, string, ...int) (<-chan coretypes.ResultEvent, error) {
	return nil, nil
}

func (c *passiveEventClient) Unsubscribe(context.Context, string, string) error {
	return nil
}

func (c *passiveEventClient) UnsubscribeAll(context.Context, string) error {
	return nil
}

func TestEventClientForStreamsStartsStoppedClient(t *testing.T) {
	client := &fakeEventClient{}

	evtClient, err := eventClientForStreams(client)
	if err != nil {
		t.Fatalf("eventClientForStreams returned error: %v", err)
	}
	if evtClient != client {
		t.Fatalf("expected returned event client to match input, got %T", evtClient)
	}
	if client.startCalls != 1 {
		t.Fatalf("expected one Start call, got %d", client.startCalls)
	}
	if !client.running {
		t.Fatal("expected client to be running after Start")
	}
}

func TestEventClientForStreamsSkipsStartForRunningClient(t *testing.T) {
	client := &fakeEventClient{running: true}

	evtClient, err := eventClientForStreams(client)
	if err != nil {
		t.Fatalf("eventClientForStreams returned error: %v", err)
	}
	if evtClient != client {
		t.Fatalf("expected returned event client to match input, got %T", evtClient)
	}
	if client.startCalls != 0 {
		t.Fatalf("expected no Start calls, got %d", client.startCalls)
	}
}

func TestEventClientForStreamsReturnsStartError(t *testing.T) {
	expectedErr := errors.New("ws unavailable")
	client := &fakeEventClient{startErr: expectedErr}

	evtClient, err := eventClientForStreams(client)
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected start error %v, got %v", expectedErr, err)
	}
	if evtClient != nil {
		t.Fatalf("expected nil event client on start failure, got %T", evtClient)
	}
	if client.startCalls != 1 {
		t.Fatalf("expected one Start call, got %d", client.startCalls)
	}
}

func TestEventClientForStreamsAllowsPassiveEventClient(t *testing.T) {
	client := &passiveEventClient{}

	evtClient, err := eventClientForStreams(client)
	if err != nil {
		t.Fatalf("eventClientForStreams returned error: %v", err)
	}
	if evtClient != client {
		t.Fatalf("expected returned event client to match input, got %T", evtClient)
	}
}

func TestEventClientForStreamsReturnsNilForNonEventClient(t *testing.T) {
	evtClient, err := eventClientForStreams(struct{}{})
	if err != nil {
		t.Fatalf("eventClientForStreams returned error: %v", err)
	}
	if evtClient != nil {
		t.Fatalf("expected nil event client, got %T", evtClient)
	}
}

func TestSubscriptionResponsePreservesRequestID(t *testing.T) {
	tests := []struct {
		name    string
		request string
		wantID  string
	}{
		{
			name:    "string ID",
			request: `{"jsonrpc":"2.0","id":"client-1","method":"eth_subscribe","params":["newHeads"]}`,
			wantID:  `"client-1"`,
		},
		{
			name:    "large numeric ID",
			request: `{"jsonrpc":"2.0","id":922337203685477580712345,"method":"eth_subscribe","params":["newHeads"]}`,
			wantID:  `922337203685477580712345`,
		},
		{
			name:    "null ID",
			request: `{"jsonrpc":"2.0","id":null,"method":"eth_subscribe","params":["newHeads"]}`,
			wantID:  `null`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msg wsRPCRequest
			if err := sonic.Unmarshal([]byte(tt.request), &msg); err != nil {
				t.Fatalf("unmarshal request: %v", err)
			}

			response, err := sonic.Marshal(&SubscriptionResponseJSON{
				Jsonrpc: "2.0",
				ID:      msg.ID,
				Result:  "0x1",
			})
			if err != nil {
				t.Fatalf("marshal response: %v", err)
			}

			var decoded struct {
				ID json.RawMessage `json:"id"`
			}
			if err := sonic.Unmarshal(response, &decoded); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if got := string(decoded.ID); got != tt.wantID {
				t.Fatalf("expected response id %s, got %s in %s", tt.wantID, got, response)
			}
		})
	}
}

func TestHandleHTTPServerExitTreatsServerClosedAsGraceful(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	done := make(chan struct{})

	err := handleHTTPServerExit(logger, done, http.ErrServerClosed)
	if err != nil {
		t.Fatalf("expected nil error for graceful shutdown, got %v", err)
	}

	select {
	case <-done:
	default:
		t.Fatal("expected done channel to be closed")
	}

	output := logs.String()
	if !strings.Contains(output, "level=INFO") {
		t.Fatalf("expected info log, got %q", output)
	}
	if !strings.Contains(output, "JSON-RPC server stopped") {
		t.Fatalf("expected graceful shutdown message, got %q", output)
	}
	if strings.Contains(output, "failed to start JSON-RPC server") {
		t.Fatalf("unexpected startup error log: %q", output)
	}
}

func TestHandleHTTPServerExitReturnsServeError(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	done := make(chan struct{})
	expectedErr := errors.New("listen tcp: address already in use")

	err := handleHTTPServerExit(logger, done, expectedErr)
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected error %v, got %v", expectedErr, err)
	}

	select {
	case <-done:
		t.Fatal("did not expect done channel to be closed")
	default:
	}

	output := logs.String()
	if !strings.Contains(output, "level=ERROR") {
		t.Fatalf("expected error log, got %q", output)
	}
	if !strings.Contains(output, "failed to start JSON-RPC server") {
		t.Fatalf("expected startup error message, got %q", output)
	}
}
