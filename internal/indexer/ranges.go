package indexer

import (
	"context"
	"sort"

	dbm "github.com/cosmos/cosmos-db"
	"upd.dev/xlab/gotracer"
)

var txIndexerTraceTag = gotracer.NewTag("component", "tx_indexer")

// BlockRange represents an inclusive range of block heights.
type BlockRange struct {
	Start int64
	End   int64
}

// LoadIndexedRanges scans the indexer DB and groups indexed block heights into ranges.
func LoadIndexedRanges(db dbm.DB) ([]BlockRange, error) {
	ctx := context.Background()
	defer gotracer.Traceless(&ctx, txIndexerTraceTag)()

	it, err := db.Iterator([]byte{KeyPrefixBlockMeta}, []byte{KeyPrefixBlockMeta + 1})
	if err != nil {
		return nil, err
	}
	defer it.Close()

	var (
		ranges    []BlockRange
		lastBlock int64 = -1
		current   BlockRange
	)

	for ; it.Valid(); it.Next() {
		height, err := parseHeightFromHeightKey(it.Key(), KeyPrefixBlockMeta)
		if err != nil {
			return nil, err
		}
		if height == lastBlock {
			continue
		}
		if lastBlock == -1 {
			current = BlockRange{Start: height, End: height}
			lastBlock = height
			continue
		}
		if height == lastBlock+1 {
			current.End = height
			lastBlock = height
			continue
		}
		ranges = append(ranges, current)
		current = BlockRange{Start: height, End: height}
		lastBlock = height
	}

	if lastBlock != -1 {
		ranges = append(ranges, current)
		return ranges, nil
	}

	// Backward compatibility for databases that only have tx-index keys.
	legacyIt, err := db.Iterator([]byte{KeyPrefixTxIndex}, []byte{KeyPrefixTxIndex + 1})
	if err != nil {
		return nil, err
	}
	defer legacyIt.Close()

	lastBlock = -1
	for ; legacyIt.Valid(); legacyIt.Next() {
		height, err := parseBlockNumberFromKey(legacyIt.Key())
		if err != nil {
			return nil, err
		}
		if height == lastBlock {
			continue
		}
		if lastBlock == -1 {
			current = BlockRange{Start: height, End: height}
			lastBlock = height
			continue
		}
		if height == lastBlock+1 {
			current.End = height
			lastBlock = height
			continue
		}
		ranges = append(ranges, current)
		current = BlockRange{Start: height, End: height}
		lastBlock = height
	}

	if lastBlock != -1 {
		ranges = append(ranges, current)
	}
	return ranges, nil
}

// ComputeGaps returns missing ranges between start and end, inclusive.
func ComputeGaps(start, end int64, ranges []BlockRange) []BlockRange {
	if start > end {
		return nil
	}
	if len(ranges) == 0 {
		return []BlockRange{{Start: start, End: end}}
	}

	// Ensure ranges are sorted and normalized.
	sort.Slice(ranges, func(i, j int) bool {
		return ranges[i].Start < ranges[j].Start
	})

	cursor := start
	var gaps []BlockRange

	for _, r := range ranges {
		if r.End < cursor {
			continue
		}
		if r.Start > end {
			break
		}
		if r.Start > cursor {
			gapEnd := r.Start - 1
			if gapEnd > end {
				gapEnd = end
			}
			if cursor <= gapEnd {
				gaps = append(gaps, BlockRange{Start: cursor, End: gapEnd})
			}
		}
		if r.End+1 > cursor {
			cursor = r.End + 1
		}
		if cursor > end {
			break
		}
	}

	if cursor <= end {
		gaps = append(gaps, BlockRange{Start: cursor, End: end})
	}

	return gaps
}

// NormalizeRanges sorts ranges and merges overlapping or adjacent segments.
func NormalizeRanges(ranges []BlockRange) []BlockRange {
	if len(ranges) == 0 {
		return nil
	}

	out := append([]BlockRange(nil), ranges...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start == out[j].Start {
			return out[i].End < out[j].End
		}
		return out[i].Start < out[j].Start
	})

	merged := make([]BlockRange, 0, len(out))
	current := out[0]

	for _, next := range out[1:] {
		if next.Start <= current.End+1 {
			if next.End > current.End {
				current.End = next.End
			}
			continue
		}

		merged = append(merged, current)
		current = next
	}

	merged = append(merged, current)
	return merged
}

// CountBlocks returns the total number of block heights covered by ranges.
func CountBlocks(ranges []BlockRange) int64 {
	var total int64
	for _, r := range ranges {
		total += r.End - r.Start + 1
	}
	return total
}
