package backend

import (
	"errors"

	rpctypes "github.com/InjectiveLabs/web3-gateway/internal/evm/rpc/types"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// SendTransaction sends transaction based on received args using Node's key to sign it
func (b *Backend) SendTransaction(args rpctypes.TransactionArgs) (common.Hash, error) {
	return common.Hash{}, errors.New("eth_sendTransaction is disabled: keyring-backed signing APIs are not supported")
}

// Sign signs the provided data using the private key of address via Geth's signature standard.
func (b *Backend) Sign(address common.Address, data hexutil.Bytes) (hexutil.Bytes, error) {
	return nil, errors.New("eth_sign is disabled: keyring-backed signing APIs are not supported")
}

// SignTypedData signs EIP-712 conformant typed data
func (b *Backend) SignTypedData(address common.Address, typedData apitypes.TypedData) (hexutil.Bytes, error) {
	return nil, errors.New("eth_signTypedData is disabled: keyring-backed signing APIs are not supported")
}
