// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/// @notice Event-only ABI for native x/bank transfer logs synthesized by
/// evm-gateway when WEB3INJ_VIRTUALIZE_COSMOS_EVENTS is enabled.
/// Logs are emitted from the reserved pseudo-address
/// 0x0000000000000000000000000000000000000800.
interface IInjectiveNativeBankTransfers {
    event NativeBankTransfer(bytes32 indexed sender, bytes32 indexed recipient, string denom, uint256 amount);
    event NativeBankCoinSpent(bytes32 indexed spender, string denom, uint256 amount);
    event NativeBankCoinReceived(bytes32 indexed receiver, string denom, uint256 amount);
    event NativeBankCoinbase(bytes32 indexed minter, string denom, uint256 amount);
    event NativeBankBurn(bytes32 indexed burner, string denom, uint256 amount);
}
