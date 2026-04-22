package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/ethereum/go-ethereum/common"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
	"github.com/InjectiveLabs/evm-gateway/internal/indexer"
)

var kvCapnpMagic = []byte{0x89, 'e', 'g', 'c', 'p', '1', '\r', '\n'}

type prefixInfo struct {
	name        string
	heightKey   bool
	hashValue   bool
	rawValue    bool
	jsonLegacy  bool
	protoLegacy bool
	traceLegacy bool
}

var prefixes = map[byte]prefixInfo{
	indexer.KeyPrefixTxHash:       {name: "tx_result", protoLegacy: true},
	indexer.KeyPrefixTxIndex:      {name: "tx_index", heightKey: true, hashValue: true, rawValue: true},
	indexer.KeyPrefixRPCtxHash:    {name: "rpc_tx", jsonLegacy: true},
	indexer.KeyPrefixRPCtxIndex:   {name: "rpc_tx_index", heightKey: true, hashValue: true, rawValue: true},
	indexer.KeyPrefixReceipt:      {name: "receipt", jsonLegacy: true},
	indexer.KeyPrefixBlockLogs:    {name: "block_logs", heightKey: true, jsonLegacy: true},
	indexer.KeyPrefixBlockMeta:    {name: "block_meta", heightKey: true, jsonLegacy: true},
	indexer.KeyPrefixBlockHash:    {name: "block_hash", rawValue: true},
	indexer.KeyPrefixTraceTx:      {name: "trace_tx", traceLegacy: true},
	indexer.KeyPrefixTraceBlock:   {name: "trace_block", heightKey: true, traceLegacy: true},
	indexer.KeyPrefixVirtualRPCtx: {name: "virtual_rpc_tx", rawValue: true},
}

type encodingCounts map[string]int64

type prefixStats struct {
	info      prefixInfo
	total     int64
	encoding  encodingCounts
	minHeight int64
	maxHeight int64
	hasHeight bool
}

type rangeStats struct {
	start int64
	end   int64
	items map[string]encodingCounts
}

// main opens the configured indexer database and dispatches the requested KV
// inspection command.
func main() {
	log.SetFlags(0)

	root := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	envFile := root.String("env-file", ".env", "path to .env file with WEB3INJ_ variables")
	dataDir := root.String("data-dir", "", "override indexer data dir")
	backend := root.String("db-backend", "", "override DB backend")
	if err := root.Parse(os.Args[1:]); err != nil {
		log.Fatal(err)
	}

	if root.NArg() == 0 {
		usage(root)
		os.Exit(2)
	}

	cfg, err := config.Load(*envFile)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *backend != "" {
		cfg.DBBackend = *backend
	}

	db, err := openIndexerDB(cfg.DataDir, cfg.DBBackend)
	if err != nil {
		log.Fatalf("open indexer DB: %v", err)
	}
	defer db.Close()

	switch root.Arg(0) {
	case "summary":
		if err := runSummary(db); err != nil {
			log.Fatal(err)
		}
	case "range":
		if root.NArg() != 2 {
			log.Fatal("usage: kvinspect range START:END")
		}
		start, end, err := parseRange(root.Arg(1))
		if err != nil {
			log.Fatal(err)
		}
		if err := runRange(db, start, end); err != nil {
			log.Fatal(err)
		}
	case "find-json-range":
		fs := flag.NewFlagSet("find-json-range", flag.ExitOnError)
		blocks := fs.Int("blocks", 1000, "number of contiguous JSON block_meta entries")
		seed := fs.Int64("seed", time.Now().UnixNano(), "random seed")
		if err := fs.Parse(root.Args()[1:]); err != nil {
			log.Fatal(err)
		}
		if *blocks <= 0 {
			log.Fatal("blocks must be positive")
		}
		if err := runFindJSONRange(db, *blocks, *seed); err != nil {
			log.Fatal(err)
		}
	default:
		usage(root)
		os.Exit(2)
	}
}

// usage prints the supported kvinspect commands and shared flags.
func usage(fs *flag.FlagSet) {
	fmt.Fprintf(fs.Output(), "usage: %s [flags] summary\n", fs.Name())
	fmt.Fprintf(fs.Output(), "       %s [flags] range START:END\n", fs.Name())
	fmt.Fprintf(fs.Output(), "       %s [flags] find-json-range [-blocks 1000] [-seed N]\n", fs.Name())
	fs.PrintDefaults()
}

// openIndexerDB opens the evmindexer database below the gateway data directory.
func openIndexerDB(rootDir string, backend string) (dbm.DB, error) {
	return dbm.NewDB("evmindexer", dbm.BackendType(backend), filepath.Join(rootDir, "data"))
}

// runSummary scans every KV entry and prints per-prefix encoding and height
// coverage information.
func runSummary(db dbm.DB) error {
	stats := make(map[byte]*prefixStats)
	for prefix, info := range prefixes {
		stats[prefix] = &prefixStats{
			info:      info,
			encoding:  make(encodingCounts),
			minHeight: 1<<63 - 1,
		}
	}

	it, err := db.Iterator(nil, nil)
	if err != nil {
		return err
	}
	defer it.Close()

	for ; it.Valid(); it.Next() {
		key := it.Key()
		value := it.Value()
		if len(key) == 0 {
			continue
		}
		prefix := key[0]
		stat, ok := stats[prefix]
		if !ok {
			stat = &prefixStats{
				info:      prefixInfo{name: fmt.Sprintf("unknown_%d", prefix)},
				encoding:  make(encodingCounts),
				minHeight: 1<<63 - 1,
			}
			stats[prefix] = stat
		}
		stat.total++
		stat.encoding[classifyValue(prefix, value)]++
		if height, ok := heightFromKey(prefix, key); ok {
			stat.hasHeight = true
			if height < stat.minHeight {
				stat.minHeight = height
			}
			if height > stat.maxHeight {
				stat.maxHeight = height
			}
		}
	}

	fmt.Println("prefix,total,encodings,min_height,max_height")
	keys := make([]int, 0, len(stats))
	for prefix := range stats {
		keys = append(keys, int(prefix))
	}
	sort.Ints(keys)
	for _, k := range keys {
		prefix := byte(k)
		stat := stats[prefix]
		if stat.total == 0 {
			continue
		}
		minHeight, maxHeight := "", ""
		if stat.hasHeight {
			minHeight = strconv.FormatInt(stat.minHeight, 10)
			maxHeight = strconv.FormatInt(stat.maxHeight, 10)
		}
		fmt.Printf("%d:%s,%d,%s,%s,%s\n",
			prefix,
			stat.info.name,
			stat.total,
			formatCounts(stat.encoding),
			minHeight,
			maxHeight,
		)
	}
	return nil
}

// runRange inspects cache records for a contiguous block range and reports
// their stored encodings.
func runRange(db dbm.DB, start, end int64) error {
	stats := rangeStats{
		start: start,
		end:   end,
		items: make(map[string]encodingCounts),
	}

	for height := start; height <= end; height++ {
		countHeightValue(db, &stats, "block_meta", indexer.BlockMetaKey(height), indexer.KeyPrefixBlockMeta)
		countHeightValue(db, &stats, "block_logs", indexer.BlockLogsKey(height), indexer.KeyPrefixBlockLogs)

		txHashes, err := hashesByHeight(db, indexer.KeyPrefixTxIndex, height)
		if err != nil {
			return fmt.Errorf("tx index %d: %w", height, err)
		}
		for _, hash := range txHashes {
			countHashValue(db, &stats, "tx_result", indexer.TxHashKey(hash), indexer.KeyPrefixTxHash)
			countHashValue(db, &stats, "receipt", indexer.ReceiptKey(hash), indexer.KeyPrefixReceipt)
			countHashValue(db, &stats, "rpc_tx", indexer.RPCtxHashKey(hash), indexer.KeyPrefixRPCtxHash)
		}

		rpcHashes, err := hashesByHeight(db, indexer.KeyPrefixRPCtxIndex, height)
		if err != nil {
			return fmt.Errorf("rpc tx index %d: %w", height, err)
		}
		for _, hash := range rpcHashes {
			countHashValue(db, &stats, "receipt", indexer.ReceiptKey(hash), indexer.KeyPrefixReceipt)
			countHashValue(db, &stats, "rpc_tx", indexer.RPCtxHashKey(hash), indexer.KeyPrefixRPCtxHash)
		}
	}

	fmt.Printf("range=%d:%d blocks=%d\n", start, end, end-start+1)
	names := make([]string, 0, len(stats.items))
	for name := range stats.items {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%s %s\n", name, formatCounts(stats.items[name]))
	}
	return nil
}

// runFindJSONRange picks a contiguous range whose block metadata still uses the
// legacy JSON encoding.
func runFindJSONRange(db dbm.DB, blocks int, seed int64) error {
	type candidate struct {
		start int64
		end   int64
	}
	var candidates []candidate
	var current candidate
	var inRun bool
	var last int64

	it, err := db.Iterator([]byte{indexer.KeyPrefixBlockMeta}, []byte{indexer.KeyPrefixBlockMeta + 1})
	if err != nil {
		return err
	}
	defer it.Close()

	flush := func() {
		if inRun && current.end-current.start+1 >= int64(blocks) {
			candidates = append(candidates, current)
		}
		inRun = false
	}

	for ; it.Valid(); it.Next() {
		height, ok := heightFromKey(indexer.KeyPrefixBlockMeta, it.Key())
		if !ok {
			continue
		}
		if classifyValue(indexer.KeyPrefixBlockMeta, it.Value()) != "json" {
			flush()
			continue
		}
		if !inRun || height != last+1 {
			flush()
			current = candidate{start: height, end: height}
			inRun = true
		} else {
			current.end = height
		}
		last = height
	}
	flush()

	if len(candidates) == 0 {
		return fmt.Errorf("no contiguous JSON block_meta range with at least %d blocks", blocks)
	}

	rng := rand.New(rand.NewSource(seed))
	picked := candidates[rng.Intn(len(candidates))]
	maxOffset := picked.end - picked.start + 1 - int64(blocks)
	offset := int64(0)
	if maxOffset > 0 {
		offset = rng.Int63n(maxOffset + 1)
	}
	start := picked.start + offset
	end := start + int64(blocks) - 1

	fmt.Printf("seed=%d candidates=%d chosen=%d:%d source_run=%d:%d source_run_blocks=%d\n",
		seed,
		len(candidates),
		start,
		end,
		picked.start,
		picked.end,
		picked.end-picked.start+1,
	)
	return nil
}

// countHeightValue records the encoding for a height-addressed cache value.
func countHeightValue(db dbm.DB, stats *rangeStats, name string, key []byte, prefix byte) {
	value, err := db.Get(key)
	if err != nil {
		stats.add(name, "read_error")
		return
	}
	if len(value) == 0 {
		stats.add(name, "missing")
		return
	}
	stats.add(name, classifyValue(prefix, value))
}

// countHashValue records the encoding for a hash-addressed cache value.
func countHashValue(db dbm.DB, stats *rangeStats, name string, key []byte, prefix byte) {
	value, err := db.Get(key)
	if err != nil {
		stats.add(name, "read_error")
		return
	}
	if len(value) == 0 {
		stats.add(name, "missing")
		return
	}
	stats.add(name, classifyValue(prefix, value))
}

// add increments the observed encoding count for a named cache item.
func (s *rangeStats) add(name, encoding string) {
	counts := s.items[name]
	if counts == nil {
		counts = make(encodingCounts)
		s.items[name] = counts
	}
	counts[encoding]++
}

// hashesByHeight returns tx hashes stored under a height-prefixed index.
func hashesByHeight(db dbm.DB, prefix byte, height int64) ([]common.Hash, error) {
	start := heightPrefix(prefix, height)
	end := heightPrefix(prefix, height+1)
	it, err := db.Iterator(start, end)
	if err != nil {
		return nil, err
	}
	defer it.Close()

	hashes := make([]common.Hash, 0)
	for ; it.Valid(); it.Next() {
		value := it.Value()
		if len(value) == common.HashLength {
			hashes = append(hashes, common.BytesToHash(value))
		}
	}
	return hashes, nil
}

// parseRange parses either a single height or a START:END height range.
func parseRange(raw string) (int64, int64, error) {
	startRaw, endRaw, ok := strings.Cut(raw, ":")
	if !ok {
		height, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return 0, 0, err
		}
		return height, height, nil
	}
	start, err := strconv.ParseInt(startRaw, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	end, err := strconv.ParseInt(endRaw, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	if start > end {
		return 0, 0, fmt.Errorf("start %d is greater than end %d", start, end)
	}
	return start, end, nil
}

// classifyValue labels a raw KV value as Cap'n Proto, legacy JSON/proto, raw,
// or unknown based on its prefix metadata and payload header.
func classifyValue(prefix byte, value []byte) string {
	if bytes.HasPrefix(value, kvCapnpMagic) {
		return "capnp"
	}
	info := prefixes[prefix]
	trimmed := bytes.TrimSpace(value)
	if info.rawValue {
		return "raw"
	}
	if info.jsonLegacy && len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		return "json"
	}
	if info.traceLegacy {
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[' || bytes.Equal(trimmed, []byte("null"))) {
			return "raw_json"
		}
		return "raw"
	}
	if info.protoLegacy {
		return "legacy_proto"
	}
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		return "json"
	}
	return "unknown"
}

// heightFromKey extracts the height portion from height-addressed indexer keys.
func heightFromKey(prefix byte, key []byte) (int64, bool) {
	info, ok := prefixes[prefix]
	if !ok || !info.heightKey || len(key) < 9 {
		return 0, false
	}
	return int64(binary.BigEndian.Uint64(key[1:9])), true
}

// heightPrefix builds the inclusive iterator prefix for height-addressed keys.
func heightPrefix(prefix byte, height int64) []byte {
	out := make([]byte, 9)
	out[0] = prefix
	binary.BigEndian.PutUint64(out[1:], uint64(height))
	return out
}

// formatCounts renders encoding counts in stable key order for CLI output.
func formatCounts(counts encodingCounts) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ";")
}
