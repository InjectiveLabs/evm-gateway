package jsonrpc

import (
	"context"
	"errors"
	"testing"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"
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
