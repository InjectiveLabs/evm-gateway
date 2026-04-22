package jsonrpc

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/pkg/errors"
)

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

func TestSubscriptionNotificationIncludesLatencyMetadata(t *testing.T) {
	emittedAt := time.Unix(0, 123456789)
	notification, err := sonic.Marshal(&SubscriptionNotification{
		Jsonrpc: "2.0",
		Method:  "eth_subscription",
		Params: &SubscriptionResult{
			Subscription: "0x1",
			Result:       map[string]string{"number": "0x2"},
			Metadata: SubscriptionMetadata{
				EmittedAtUnixNano: emittedAt.UnixNano(),
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}

	var decoded struct {
		Params struct {
			Metadata struct {
				EmittedAtUnixNano int64 `json:"emittedAtUnixNano"`
			} `json:"metadata"`
		} `json:"params"`
	}
	if err := sonic.Unmarshal(notification, &decoded); err != nil {
		t.Fatalf("unmarshal notification: %v", err)
	}
	if decoded.Params.Metadata.EmittedAtUnixNano != emittedAt.UnixNano() {
		t.Fatalf("unexpected emitted timestamp: got %d want %d", decoded.Params.Metadata.EmittedAtUnixNano, emittedAt.UnixNano())
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
