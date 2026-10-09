package types

import (
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

func TestTxAnteFailed(t *testing.T) {
	anteEvents := []abci.Event{{Type: "coin_spent"}, {Type: "transfer"}, {Type: "tx"}}
	testCases := []struct {
		name string
		res  *abci.ExecTxResult
		want bool
	}{
		{name: "nil result", res: nil, want: false},
		{name: "success", res: &abci.ExecTxResult{Code: abci.CodeTypeOK}, want: false},
		{name: "evm codespace failure", res: &abci.ExecTxResult{Code: 3, Codespace: evmtypes.ModuleName}, want: false},
		{name: "legacy block gas limit", res: &abci.ExecTxResult{Code: 11, Codespace: "sdk", Log: ExceedBlockGasLimitError + " 1"}, want: false},
		{
			name: "message execution failure",
			res:  &abci.ExecTxResult{Code: 1, Codespace: "undefined", Log: "failed to execute message; message index: 0: reverted", Events: anteEvents},
			want: false,
		},
		{
			name: "panic during message execution keeps ante events",
			res:  &abci.ExecTxResult{Code: 111222, Codespace: "undefined", Log: "recovered: runtime error: invalid memory address", Events: anteEvents},
			want: false,
		},
		{
			name: "ante insufficient funds (mainnet 185556799 tx 131)",
			res: &abci.ExecTxResult{
				Code:      5,
				Codespace: "sdk",
				Log:       "failed to transfer 60080000000000000 from address 0xA193925D4D6702d1595A0de635C913B06Bf38Fb0 using the EVM block context transfer function: insufficient funds",
			},
			want: true,
		},
		{name: "ante invalid nonce", res: &abci.ExecTxResult{Code: 32, Codespace: "sdk", Log: "invalid nonce"}, want: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TxAnteFailed(tc.res); got != tc.want {
				t.Fatalf("TxAnteFailed = %v want %v", got, tc.want)
			}
		})
	}
}
