package jsonrpc

import (
	"net/http"

	"github.com/bytedance/sonic"

	"github.com/InjectiveLabs/evm-gateway/internal/syncstatus"
)

func makeSyncStatusHandler(status *syncstatus.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = sonic.ConfigDefault.NewEncoder(w).Encode(status.Snapshot())
	}
}
