using Go = import "/go.capnp";

@0xbc3f21e1d17a526c;

$Go.package("kvcapnp");
$Go.import("github.com/InjectiveLabs/evm-gateway/internal/indexer/kvcapnp");

struct BlockMeta {
  height @0 :Int64;
  hash @1 :Text;
  parentHash @2 :Text;
  stateRoot @3 :Text;
  miner @4 :Text;
  timestamp @5 :Int64;
  size @6 :UInt64;
  gasLimit @7 :UInt64;
  gasUsed @8 :UInt64;
  ethTxCount @9 :Int32;
  txCount @10 :Int32;
  bloom @11 :Text;
  transactionsRoot @12 :Text;
  baseFee @13 :Text;
  virtualizedCosmosEvents @14 :Bool;
}

struct Log {
  address @0 :Data;
  topics @1 :List(Data);
  data @2 :Data;
  blockNumber @3 :UInt64;
  txHash @4 :Data;
  txIndex @5 :UInt64;
  blockHash @6 :Data;
  index @7 :UInt64;
  removed @8 :Bool;
  virtual @9 :Bool;
  cosmosHash @10 :Data;
}

struct LogGroup {
  logs @0 :List(Log);
}

struct BlockLogs {
  groups @0 :List(LogGroup);
}

struct Receipt {
  status @0 :UInt64;
  cumulativeGasUsed @1 :UInt64;
  gasUsed @2 :UInt64;
  reason @3 :Text;
  vmError @4 :Text;
  logsBloom @5 :Data;
  logs @6 :List(Log);
  transactionHash @7 :Data;
  contractAddress @8 :Data;
  blockHash @9 :Data;
  blockNumber @10 :UInt64;
  transactionIndex @11 :UInt64;
  effectiveGasPrice @12 :Data;
  from @13 :Data;
  to @14 :Data;
  type @15 :UInt64;
  reasonPresent @16 :Bool;
  vmErrorPresent @17 :Bool;
  effectiveGasPricePresent @18 :Bool;
}

struct AccessTuple {
  address @0 :Data;
  storageKeys @1 :List(Data);
}

struct RPCTransaction {
  blockHash @0 :Data;
  blockNumber @1 :Data;
  from @2 :Data;
  gas @3 :UInt64;
  gasPrice @4 :Data;
  gasFeeCap @5 :Data;
  gasTipCap @6 :Data;
  hash @7 :Data;
  input @8 :Data;
  nonce @9 :UInt64;
  to @10 :Data;
  transactionIndex @11 :UInt64;
  value @12 :Data;
  type @13 :UInt64;
  accesses @14 :List(AccessTuple);
  chainId @15 :Data;
  v @16 :Data;
  r @17 :Data;
  s @18 :Data;
  virtual @19 :Bool;
  cosmosHash @20 :Data;
  transactionIndexPresent @21 :Bool;
  blockNumberPresent @22 :Bool;
  gasPricePresent @23 :Bool;
  gasFeeCapPresent @24 :Bool;
  gasTipCapPresent @25 :Bool;
  valuePresent @26 :Bool;
  accessesPresent @27 :Bool;
  chainIdPresent @28 :Bool;
  vPresent @29 :Bool;
  rPresent @30 :Bool;
  sPresent @31 :Bool;
}

struct TracePayload {
  raw @0 :Data;
}

struct TxResult {
  height @0 :Int64;
  txIndex @1 :UInt32;
  msgIndex @2 :UInt32;
  ethTxIndex @3 :Int32;
  failed @4 :Bool;
  gasUsed @5 :UInt64;
  cumulativeGasUsed @6 :UInt64;
}
