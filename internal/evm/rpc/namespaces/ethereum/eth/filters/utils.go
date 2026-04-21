package filters

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/filters"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
)

const (
	maxAddresses = 32
	maxTopics    = 4
)

// FilterLogs creates a slice of logs matching the given criteria.
// [] -> anything
// [A] -> A in first position of log topics, anything after
// [null, B] -> anything in first position, B in second position
// [A, B] -> A in first position and B in second position
// [[A, B], [A, B]] -> A or B in first position, A or B in second position
func FilterLogs(logs []*virtualbank.RPCLog, fromBlock, toBlock *big.Int, addresses []common.Address, topics [][]common.Hash) []*virtualbank.RPCLog {
	var ret []*virtualbank.RPCLog
	for _, log := range logs {
		if fromBlock != nil && fromBlock.Int64() >= 0 && fromBlock.Uint64() > uint64(log.BlockNumber) {
			continue
		}
		if toBlock != nil && toBlock.Int64() >= 0 && toBlock.Uint64() < uint64(log.BlockNumber) {
			continue
		}
		if !virtualbank.LogMatches(log, addresses, topics) {
			continue
		}
		ret = append(ret, log)
	}
	return ret
}

// https://github.com/ethereum/go-ethereum/blob/v1.10.14/eth/filters/filter.go#L321
func bloomFilter(bloom ethtypes.Bloom, addresses []common.Address, topics [][]common.Hash) bool {
	if len(addresses) > 0 {
		var included bool
		for _, addr := range addresses {
			if ethtypes.BloomLookup(bloom, addr) {
				included = true
				break
			}
		}
		if !included {
			return false
		}
	}

	for _, sub := range topics {
		included := len(sub) == 0 // empty rule set == wildcard
		for _, topic := range sub {
			if ethtypes.BloomLookup(bloom, topic) {
				included = true
				break
			}
		}
		if !included {
			return false
		}
	}
	return true
}

// returnHashes is a helper that will return an empty hash array case the given hash array is nil,
// otherwise the given hashes array is returned.
func returnHashes(hashes []common.Hash) []common.Hash {
	if hashes == nil {
		return []common.Hash{}
	}
	return hashes
}

// returnLogs is a helper that will return an empty log array in case the given logs array is nil,
// otherwise the given logs array is returned.
func returnLogs(logs []*virtualbank.RPCLog) []*virtualbank.RPCLog {
	if logs == nil {
		return []*virtualbank.RPCLog{}
	}
	return logs
}

func ValidateFilterCriteria(criteria filters.FilterCriteria) error {
	if len(criteria.Addresses) > maxAddresses {
		return fmt.Errorf("max filter addresses exceeded: %d > %d", len(criteria.Addresses), maxAddresses)
	}
	if len(criteria.Topics) > maxTopics {
		return fmt.Errorf("max filter topics exceeded: %d > %d", len(criteria.Topics), maxTopics)
	}
	return nil
}
