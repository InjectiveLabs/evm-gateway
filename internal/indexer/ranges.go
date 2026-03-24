package indexer

import (
	"sort"

	dbm "github.com/cosmos/cosmos-db"
)

// BlockRange represents an inclusive range of block heights.
type BlockRange struct {
	Start int64
	End   int64
}

// LoadIndexedRanges scans the indexer DB and groups indexed block heights into ranges.
func LoadIndexedRanges(db dbm.DB) ([]BlockRange, error) {
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
