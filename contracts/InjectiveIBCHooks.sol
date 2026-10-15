// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/// @notice Event-only ABI for IBC-triggered EVM calls synthesized by
/// evm-gateway when WEB3INJ_VIRTUALIZE_COSMOS_EVENTS is enabled.
/// Logs are emitted from the reserved virtual address
/// 0x0000000000000000000000000000000000000800 (shared with virtual bank
/// transfers), never from the real IBC precompile address 0x...69.
interface IInjectiveIBCHooks {
    event IBCHookCall(
        string destinationPort,
        string destinationChannel,
        uint64 sequence,
        address indexed contractAddress,
        bool success,
        bytes returnData,
        string errorMessage,
        uint256 indexed msgIndex
    );
}
