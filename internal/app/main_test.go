package app

import (
	"os"
	"testing"
	"time"

	txindexer "github.com/InjectiveLabs/evm-gateway/internal/indexer"
)

func TestMain(m *testing.M) {
	txindexer.VerifyFetchAttempts = 3
	txindexer.VerifyFetchRetryDelay = time.Millisecond
	txindexer.VerifyFetchMaxRetryWait = 2 * time.Millisecond
	os.Exit(m.Run())
}
