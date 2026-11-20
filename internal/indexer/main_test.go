package indexer

import (
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	VerifyFetchAttempts = 3
	VerifyFetchRetryDelay = time.Millisecond
	VerifyFetchMaxRetryWait = 2 * time.Millisecond
	os.Exit(m.Run())
}
