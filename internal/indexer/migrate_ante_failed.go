package indexer

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	errorsmod "cosmossdk.io/errors"
	abci "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	dbm "github.com/cosmos/cosmos-db"
	sdkcodec "github.com/cosmos/cosmos-sdk/codec"
	"github.com/ethereum/go-ethereum/common"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

const (
	KeyPrefixMigration = 12

	// AnteFailedTxsMigration is the name of the migration that removes Ethereum
	// txs indexed from Cosmos txs that failed in the ante handler.
	AnteFailedTxsMigration = "ante-failed-txs-v1"

	anteFailedScanProgressEvery = 1_000_000
)

// Block results fetches during verification retry transient failures (e.g. a
// 502 from a proxy in front of an archival node) with capped exponential
// backoff, like block fetches during sync. Tunable for tests.
var (
	VerifyFetchAttempts     = 10
	VerifyFetchRetryDelay   = 300 * time.Millisecond
	VerifyFetchMaxRetryWait = 10 * time.Second
)

// MigrationKey returns the key of the completion marker of a named migration.
func MigrationKey(name string) []byte {
	return append([]byte{KeyPrefixMigration}, []byte(name)...)
}

// AnteFailedScanOptions bounds the offline scan to an inclusive height range.
// Zero bounds are open.
type AnteFailedScanOptions struct {
	From int64
	To   int64
}

func (o AnteFailedScanOptions) contains(height int64) bool {
	if o.From > 0 && height < o.From {
		return false
	}
	if o.To > 0 && height > o.To {
		return false
	}
	return true
}

// AnteFailedCandidate is an indexed failed Ethereum tx whose stored record
// matches what older indexer versions wrote for any non-EVM failure, which
// includes ante handler failures.
type AnteFailedCandidate struct {
	Height   int64       `json:"height"`
	TxIndex  uint32      `json:"tx_index"`
	Hash     common.Hash `json:"hash"`
	GasUsed  uint64      `json:"gas_used"`
	GasLimit uint64      `json:"gas_limit"`
}

// TxHashConflict is a block listing a tx hash whose hash-keyed records are
// owned by another height (or missing).
type TxHashConflict struct {
	Height      int64       `json:"height"`
	Hash        common.Hash `json:"hash"`
	OwnerHeight int64       `json:"owner_height"`
}

// AnteFailedScanResult is the outcome of the offline scan of the local KV
// state.
type AnteFailedScanResult struct {
	ScannedTxs     int64                 `json:"scanned_txs"`
	FailedTxs      int64                 `json:"failed_txs"`
	ScannedListing int64                 `json:"scanned_listings"`
	Candidates     []AnteFailedCandidate `json:"candidates"`
	Conflicts      []TxHashConflict      `json:"conflicts"`
}

// CandidateHeights returns the sorted unique heights of all candidates.
func (r *AnteFailedScanResult) CandidateHeights() []int64 {
	heights := make([]int64, 0, len(r.Candidates))
	for _, candidate := range r.Candidates {
		heights = append(heights, candidate.Height)
	}
	return uniqueSortedHeights(heights)
}

// ConflictHeights returns the sorted unique heights involved in hash
// ownership conflicts: the listing heights and the owner heights.
func (r *AnteFailedScanResult) ConflictHeights() []int64 {
	heights := make([]int64, 0, 2*len(r.Conflicts))
	for _, conflict := range r.Conflicts {
		heights = append(heights, conflict.Height)
		if conflict.OwnerHeight > 0 {
			heights = append(heights, conflict.OwnerHeight)
		}
	}
	return uniqueSortedHeights(heights)
}

// ScanAnteFailedCandidates scans the local KV state without any network
// access. It reports:
//   - candidates: failed txs whose stored gas used equals the tx gas limit,
//     which is what older indexer versions stored for every failure outside
//     the EVM codespace (ante handler failures included);
//   - conflicts: per-block tx listings whose hash records belong to another
//     height, i.e. a tx hash indexed in more than one block.
func ScanAnteFailedCandidates(
	ctx context.Context,
	db dbm.DB,
	codec sdkcodec.Codec,
	opts AnteFailedScanOptions,
	logger *slog.Logger,
) (*AnteFailedScanResult, error) {
	if logger == nil {
		logger = slog.Default()
	}
	result := &AnteFailedScanResult{
		Candidates: make([]AnteFailedCandidate, 0),
		Conflicts:  make([]TxHashConflict, 0),
	}

	if err := scanFailedTxRecords(ctx, db, codec, opts, logger, result); err != nil {
		return nil, err
	}
	if err := scanTxListingConflicts(ctx, db, codec, opts, logger, result); err != nil {
		return nil, err
	}

	sort.Slice(result.Candidates, func(i, j int) bool {
		if result.Candidates[i].Height != result.Candidates[j].Height {
			return result.Candidates[i].Height < result.Candidates[j].Height
		}
		return result.Candidates[i].TxIndex < result.Candidates[j].TxIndex
	})
	return result, nil
}

func scanFailedTxRecords(
	ctx context.Context,
	db dbm.DB,
	codec sdkcodec.Codec,
	opts AnteFailedScanOptions,
	logger *slog.Logger,
	result *AnteFailedScanResult,
) error {
	it, err := db.Iterator([]byte{KeyPrefixTxHash}, []byte{KeyPrefixTxHash + 1})
	if err != nil {
		return errorsmod.Wrap(err, "scan tx records")
	}
	defer it.Close()

	for ; it.Valid(); it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := it.Key()
		if len(key) != 1+common.HashLength {
			continue
		}
		result.ScannedTxs++
		if result.ScannedTxs%anteFailedScanProgressEvery == 0 {
			logger.Info("scanning indexed txs", "scanned", result.ScannedTxs, "failed", result.FailedTxs, "candidates", len(result.Candidates))
		}

		txResult, err := unmarshalTxResultPayload(codec, it.Value())
		if err != nil || txResult == nil {
			logger.Warn("skipping undecodable tx record", "key", fmt.Sprintf("%x", key), "error", err)
			continue
		}
		if !txResult.Failed || !opts.contains(txResult.Height) {
			continue
		}
		result.FailedTxs++

		hash := common.BytesToHash(key[1:])
		gasLimit, known, err := indexedTxGasLimit(db, hash)
		if err != nil {
			return err
		}
		// Older indexers stored the gas limit as gas used for non-EVM failures.
		// Without the rpc tx payload the record can't be ruled out.
		if known && txResult.GasUsed != gasLimit {
			continue
		}
		result.Candidates = append(result.Candidates, AnteFailedCandidate{
			Height:   txResult.Height,
			TxIndex:  txResult.TxIndex,
			Hash:     hash,
			GasUsed:  txResult.GasUsed,
			GasLimit: gasLimit,
		})
	}
	return nil
}

func indexedTxGasLimit(db dbm.DB, hash common.Hash) (uint64, bool, error) {
	bz, err := db.Get(RPCtxHashKey(hash))
	if err != nil {
		return 0, false, errorsmod.Wrapf(err, "get rpc tx %s", hash.Hex())
	}
	if len(bz) == 0 {
		return 0, false, nil
	}
	tx, err := unmarshalRPCTransactionPayload(bz)
	if err != nil || tx == nil {
		return 0, false, nil
	}
	return uint64(tx.Gas), true, nil
}

func scanTxListingConflicts(
	ctx context.Context,
	db dbm.DB,
	codec sdkcodec.Codec,
	opts AnteFailedScanOptions,
	logger *slog.Logger,
	result *AnteFailedScanResult,
) error {
	start := []byte{KeyPrefixTxIndex}
	end := []byte{KeyPrefixTxIndex + 1}
	if opts.From > 0 {
		start = txIndexPrefixStart(opts.From)
	}
	if opts.To > 0 {
		end = txIndexPrefixEnd(opts.To)
	}
	it, err := db.Iterator(start, end)
	if err != nil {
		return errorsmod.Wrap(err, "scan tx listings")
	}
	defer it.Close()

	for ; it.Valid(); it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		height, err := parseBlockNumberFromKey(it.Key())
		if err != nil {
			continue
		}
		result.ScannedListing++
		if result.ScannedListing%anteFailedScanProgressEvery == 0 {
			logger.Info("scanning block tx listings", "scanned", result.ScannedListing, "conflicts", len(result.Conflicts))
		}

		hash := common.BytesToHash(it.Value())
		bz, err := db.Get(TxHashKey(hash))
		if err != nil {
			return errorsmod.Wrapf(err, "get tx record %s", hash.Hex())
		}
		var owner int64
		if len(bz) > 0 {
			txResult, err := unmarshalTxResultPayload(codec, bz)
			if err == nil && txResult != nil {
				owner = txResult.Height
			}
		}
		if owner == height {
			continue
		}
		result.Conflicts = append(result.Conflicts, TxHashConflict{
			Height:      height,
			Hash:        hash,
			OwnerHeight: owner,
		})
	}
	return nil
}

// BlockResultsFetcher fetches block results from a (archival) Comet node.
type BlockResultsFetcher interface {
	BlockResults(ctx context.Context, height *int64) (*coretypes.ResultBlockResults, error)
}

// AnteFailedVerdict explains why a candidate needs its block to be resynced.
type AnteFailedVerdict string

const (
	// VerdictAnteFailed: the Cosmos tx failed in the ante handler, the Ethereum
	// tx must not be indexed.
	VerdictAnteFailed AnteFailedVerdict = "ante_failed"
	// VerdictGasUsedMismatch: the tx failed after the ante handler and stays
	// indexed, but its stored gas used differs from the chain's.
	VerdictGasUsedMismatch AnteFailedVerdict = "gas_used_mismatch"
)

// VerifiedCandidate is a candidate confirmed by its block results.
type VerifiedCandidate struct {
	AnteFailedCandidate
	Verdict   AnteFailedVerdict `json:"verdict"`
	Code      uint32            `json:"code"`
	Codespace string            `json:"codespace"`
	Log       string            `json:"log"`
}

// AnteFailedVerification is the outcome of verifying candidates against
// block results.
type AnteFailedVerification struct {
	CheckedHeights int64               `json:"checked_heights"`
	Confirmed      []VerifiedCandidate `json:"confirmed"`
}

// Heights returns the sorted unique heights of confirmed candidates.
func (v *AnteFailedVerification) Heights() []int64 {
	heights := make([]int64, 0, len(v.Confirmed))
	for _, confirmed := range v.Confirmed {
		heights = append(heights, confirmed.Height)
	}
	return uniqueSortedHeights(heights)
}

// VerifyAnteFailedCandidates fetches block results only for candidate heights
// and keeps the candidates whose indexed record is wrong under the current
// indexing rules.
func VerifyAnteFailedCandidates(
	ctx context.Context,
	fetcher BlockResultsFetcher,
	candidates []AnteFailedCandidate,
	jobs int,
	logger *slog.Logger,
) (*AnteFailedVerification, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if jobs < 1 {
		jobs = 1
	}

	byHeight := make(map[int64][]AnteFailedCandidate)
	for _, candidate := range candidates {
		byHeight[candidate.Height] = append(byHeight[candidate.Height], candidate)
	}
	heights := make([]int64, 0, len(byHeight))
	for height := range byHeight {
		heights = append(heights, height)
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu        sync.Mutex
		firstErr  error
		confirmed = make([]VerifiedCandidate, 0)
		checked   int64
		wg        sync.WaitGroup
		heightC   = make(chan int64)
		lastLog   = time.Now()
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}

	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for height := range heightC {
				res, err := fetchBlockResultsWithRetry(ctx, fetcher, height, logger)
				if err != nil {
					fail(errorsmod.Wrapf(err, "fetch block results %d", height))
					return
				}
				verified, err := verifyHeightCandidates(res, byHeight[height])
				if err != nil {
					fail(err)
					return
				}
				mu.Lock()
				confirmed = append(confirmed, verified...)
				checked++
				if time.Since(lastLog) > 10*time.Second {
					lastLog = time.Now()
					logger.Info("verifying candidate heights", "checked", checked, "total", len(heights), "confirmed", len(confirmed))
				}
				mu.Unlock()
			}
		}()
	}

feed:
	for _, height := range heights {
		select {
		case heightC <- height:
		case <-ctx.Done():
			break feed
		}
	}
	close(heightC)
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sort.Slice(confirmed, func(i, j int) bool {
		if confirmed[i].Height != confirmed[j].Height {
			return confirmed[i].Height < confirmed[j].Height
		}
		return confirmed[i].TxIndex < confirmed[j].TxIndex
	})
	return &AnteFailedVerification{CheckedHeights: checked, Confirmed: confirmed}, nil
}

func fetchBlockResultsWithRetry(ctx context.Context, fetcher BlockResultsFetcher, height int64, logger *slog.Logger) (*coretypes.ResultBlockResults, error) {
	delay := VerifyFetchRetryDelay
	for attempt := 1; ; attempt++ {
		h := height
		res, err := fetcher.BlockResults(ctx, &h)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt >= VerifyFetchAttempts {
			return nil, err
		}
		logger.Warn("fetch block results failed; retrying", "height", height, "attempt", attempt, "retry_in", delay, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > VerifyFetchMaxRetryWait {
			delay = VerifyFetchMaxRetryWait
		}
	}
}

func verifyHeightCandidates(res *coretypes.ResultBlockResults, candidates []AnteFailedCandidate) ([]VerifiedCandidate, error) {
	if res == nil {
		return nil, fmt.Errorf("nil block results")
	}
	verified := make([]VerifiedCandidate, 0)
	for _, candidate := range candidates {
		if int(candidate.TxIndex) >= len(res.TxResults) || res.TxResults[candidate.TxIndex] == nil {
			return nil, fmt.Errorf("block %d: missing tx result %d for %s", candidate.Height, candidate.TxIndex, candidate.Hash.Hex())
		}
		txResult := res.TxResults[candidate.TxIndex]
		verdict, ok := anteFailedVerdict(txResult, candidate)
		if !ok {
			continue
		}
		verified = append(verified, VerifiedCandidate{
			AnteFailedCandidate: candidate,
			Verdict:             verdict,
			Code:                txResult.Code,
			Codespace:           txResult.Codespace,
			Log:                 truncateLog(txResult.Log, 256),
		})
	}
	return verified, nil
}

func anteFailedVerdict(txResult *abci.ExecTxResult, candidate AnteFailedCandidate) (AnteFailedVerdict, bool) {
	if rpctypes.TxAnteFailed(txResult) {
		return VerdictAnteFailed, true
	}
	if txResult.Code == abci.CodeTypeOK || txResult.Codespace == evmtypes.ModuleName || rpctypes.TxExceedBlockGasLimit(txResult) {
		return "", false
	}
	// Failed after the ante handler without EVM events: the current indexer
	// stores the chain's gas used. Multi-message txs keep the gas limit and
	// may be resynced needlessly, which is harmless.
	if txResult.GasUsed >= 0 && uint64(txResult.GasUsed) != candidate.GasUsed {
		return VerdictGasUsedMismatch, true
	}
	return "", false
}

func truncateLog(log string, limit int) string {
	if len(log) <= limit {
		return log
	}
	return log[:limit] + "..."
}

// AnteFailedRepairPlan lists the heights to resync.
type AnteFailedRepairPlan struct {
	VerifiedHeights []int64      `json:"verified_heights"`
	ConflictHeights []int64      `json:"conflict_heights"`
	Heights         []int64      `json:"heights"`
	Ranges          []BlockRange `json:"ranges"`
}

// PlanAnteFailedRepair merges verified and conflict heights into resync
// ranges.
func PlanAnteFailedRepair(scan *AnteFailedScanResult, verification *AnteFailedVerification) AnteFailedRepairPlan {
	plan := AnteFailedRepairPlan{
		VerifiedHeights: []int64{},
		ConflictHeights: []int64{},
	}
	if verification != nil {
		plan.VerifiedHeights = verification.Heights()
	}
	if scan != nil {
		plan.ConflictHeights = scan.ConflictHeights()
	}
	all := append(append([]int64{}, plan.VerifiedHeights...), plan.ConflictHeights...)
	plan.Heights = uniqueSortedHeights(all)

	ranges := make([]BlockRange, 0, len(plan.Heights))
	for _, height := range plan.Heights {
		ranges = append(ranges, BlockRange{Start: height, End: height})
	}
	plan.Ranges = NormalizeRanges(ranges)
	if plan.Ranges == nil {
		plan.Ranges = []BlockRange{}
	}
	return plan
}

func uniqueSortedHeights(heights []int64) []int64 {
	if len(heights) == 0 {
		return []int64{}
	}
	sorted := append([]int64{}, heights...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	out := sorted[:1]
	for _, height := range sorted[1:] {
		if height != out[len(out)-1] {
			out = append(out, height)
		}
	}
	return out
}

// MigrationMarker records a completed migration.
type MigrationMarker struct {
	Name          string    `json:"name"`
	CompletedAt   time.Time `json:"completed_at"`
	ResyncedCount int       `json:"resynced_count"`
	Version       string    `json:"version,omitempty"`
}

// LoadMigrationMarker returns the completion marker of a migration, if any.
func LoadMigrationMarker(db dbm.DB, name string) (*MigrationMarker, error) {
	bz, err := db.Get(MigrationKey(name))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "load migration marker %s", name)
	}
	if len(bz) == 0 {
		return nil, nil
	}
	marker, err := unmarshalJSON[MigrationMarker](bz)
	if err != nil {
		return nil, errorsmod.Wrapf(err, "decode migration marker %s", name)
	}
	return &marker, nil
}

// SaveMigrationMarker records a completed migration.
func SaveMigrationMarker(db dbm.DB, marker MigrationMarker) error {
	if err := db.SetSync(MigrationKey(marker.Name), mustJSON(marker)); err != nil {
		return errorsmod.Wrapf(err, "save migration marker %s", marker.Name)
	}
	return nil
}
