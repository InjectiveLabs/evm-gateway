package jsonrpc

import (
	"encoding/json"
	"net/http"

	"github.com/InjectiveLabs/web3-gateway/internal/syncstatus"
)

func makeSyncStatusHandler(status *syncstatus.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status.Snapshot())
	}
}
