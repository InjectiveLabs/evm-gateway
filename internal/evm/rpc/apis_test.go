package rpc

import "testing"

func TestTraceNamespaceIsRegistered(t *testing.T) {
	if TraceNamespace != "trace" {
		t.Fatalf("TraceNamespace = %q, want trace", TraceNamespace)
	}
	if _, ok := apiCreators[TraceNamespace]; !ok {
		t.Fatal("trace namespace creator is not registered")
	}
}
