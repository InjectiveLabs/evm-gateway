package types

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/holiman/uint256"
)

type recordingStateDB struct {
	nonces   map[common.Address]uint64
	codes    map[common.Address][]byte
	balances map[common.Address]*uint256.Int
	storage  map[common.Address]map[common.Hash]common.Hash
	state    map[common.Address]map[common.Hash]common.Hash
}

func newRecordingStateDB() *recordingStateDB {
	return &recordingStateDB{
		nonces:   make(map[common.Address]uint64),
		codes:    make(map[common.Address][]byte),
		balances: make(map[common.Address]*uint256.Int),
		storage:  make(map[common.Address]map[common.Hash]common.Hash),
		state:    make(map[common.Address]map[common.Hash]common.Hash),
	}
}

func (db *recordingStateDB) SetNonce(addr common.Address, nonce uint64, _ tracing.NonceChangeReason) {
	db.nonces[addr] = nonce
}

func (db *recordingStateDB) SetCode(addr common.Address, code []byte) (prev []byte) {
	db.codes[addr] = append([]byte(nil), code...)
	return nil
}

func (db *recordingStateDB) SetBalance(addr common.Address, amount *uint256.Int, _ tracing.BalanceChangeReason) {
	db.balances[addr] = new(uint256.Int).Set(amount)
}

func (db *recordingStateDB) SetStorage(addr common.Address, storage map[common.Hash]common.Hash) {
	db.storage[addr] = storage
}

func (db *recordingStateDB) SetState(addr common.Address, key, value common.Hash) common.Hash {
	if db.state[addr] == nil {
		db.state[addr] = make(map[common.Hash]common.Hash)
	}
	db.state[addr][key] = value
	return common.Hash{}
}

func TestStateOverrideApply(t *testing.T) {
	if err := (*StateOverride)(nil).Apply(newRecordingStateDB()); err != nil {
		t.Fatalf("nil override should succeed: %v", err)
	}

	addrA := common.HexToAddress("0x1000000000000000000000000000000000000001")
	addrB := common.HexToAddress("0x2000000000000000000000000000000000000002")
	nonce := hexutil.Uint64(9)
	code := hexutil.Bytes{0xaa, 0xbb}
	balance := ptrHexBig(big.NewInt(123))
	state := map[common.Hash]common.Hash{
		common.HexToHash("0x1"): common.HexToHash("0x2"),
	}
	stateDiff := map[common.Hash]common.Hash{
		common.HexToHash("0x3"): common.HexToHash("0x4"),
	}

	override := StateOverride{
		addrA: {
			Nonce:   &nonce,
			Code:    &code,
			Balance: balance,
			State:   &state,
		},
		addrB: {
			StateDiff: &stateDiff,
		},
	}

	db := newRecordingStateDB()
	if err := override.Apply(db); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	if db.nonces[addrA] != 9 {
		t.Fatalf("unexpected nonce: %d", db.nonces[addrA])
	}
	if got := db.codes[addrA]; len(got) != 2 || got[0] != 0xaa || got[1] != 0xbb {
		t.Fatalf("unexpected code: %x", got)
	}
	if got := db.balances[addrA]; got == nil || got.ToBig().Cmp(big.NewInt(123)) != 0 {
		t.Fatalf("unexpected balance: %v", got)
	}
	if got := db.storage[addrA][common.HexToHash("0x1")]; got != common.HexToHash("0x2") {
		t.Fatalf("unexpected storage value: %s", got.Hex())
	}
	if got := db.state[addrB][common.HexToHash("0x3")]; got != common.HexToHash("0x4") {
		t.Fatalf("unexpected state diff value: %s", got.Hex())
	}
}

func TestStateOverrideApplyErrors(t *testing.T) {
	addr := common.HexToAddress("0x3000000000000000000000000000000000000003")
	state := map[common.Hash]common.Hash{}
	stateDiff := map[common.Hash]common.Hash{}
	override := StateOverride{
		addr: {
			State:     &state,
			StateDiff: &stateDiff,
		},
	}
	if err := override.Apply(newRecordingStateDB()); err == nil {
		t.Fatal("expected error when both state and stateDiff are set")
	}

	huge := new(big.Int).Lsh(big.NewInt(1), 300)
	override = StateOverride{
		addr: {
			Balance: ptrHexBig(huge),
		},
	}
	if err := override.Apply(newRecordingStateDB()); err == nil {
		t.Fatal("expected overflow error")
	}
}

func TestBlockOverridesApply(t *testing.T) {
	blockCtx := &vm.BlockContext{}
	var nilOverrides *BlockOverrides
	nilOverrides.Apply(blockCtx)

	number := hexutil.Big(*big.NewInt(10))
	difficulty := hexutil.Big(*big.NewInt(20))
	timeValue := hexutil.Uint64(30)
	gasLimit := hexutil.Uint64(40)
	coinbase := common.HexToAddress("0x4000000000000000000000000000000000000004")
	random := common.HexToHash("0x5000000000000000000000000000000000000000000000000000000000000005")
	baseFee := hexutil.Big(*big.NewInt(60))

	overrides := &BlockOverrides{
		Number:     &number,
		Difficulty: &difficulty,
		Time:       &timeValue,
		GasLimit:   &gasLimit,
		Coinbase:   &coinbase,
		Random:     &random,
		BaseFee:    &baseFee,
	}
	overrides.Apply(blockCtx)

	if blockCtx.BlockNumber.Cmp(big.NewInt(10)) != 0 {
		t.Fatalf("unexpected block number: %s", blockCtx.BlockNumber)
	}
	if blockCtx.Difficulty.Cmp(big.NewInt(20)) != 0 {
		t.Fatalf("unexpected difficulty: %s", blockCtx.Difficulty)
	}
	if blockCtx.Time != 30 || blockCtx.GasLimit != 40 {
		t.Fatalf("unexpected block context numbers: %+v", blockCtx)
	}
	if blockCtx.Coinbase != coinbase {
		t.Fatalf("unexpected coinbase: %s", blockCtx.Coinbase.Hex())
	}
	if blockCtx.Random == nil || *blockCtx.Random != random {
		t.Fatalf("unexpected random: %#v", blockCtx.Random)
	}
	if blockCtx.BaseFee.Cmp(big.NewInt(60)) != 0 {
		t.Fatalf("unexpected base fee: %s", blockCtx.BaseFee)
	}
}

func ptrHexBig(v *big.Int) **hexutil.Big {
	h := hexutil.Big(*new(big.Int).Set(v))
	p := &h
	return &p
}
