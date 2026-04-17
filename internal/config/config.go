package config

import (
	"bufio"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/pkg/errors"
)

const envPrefix = "WEB3INJ_"

// Config is the top-level configuration for evm-gateway.
type Config struct {
	Env        string
	LogFormat  string
	LogVerbose bool

	ChainID                string
	EVMChainID             string
	CometRPC               string
	GRPCAddr               string
	Earliest               int64
	FetchJobs              int
	DataDir                string
	DBBackend              string
	AllowGaps              bool
	EnableSync             bool
	ParallelSyncTipAndGaps bool
	OfflineRPCOnly         bool
	MinGasPrices           string

	JSONRPC  JSONRPCConfig
	Tracing  TracingConfig
	Shutdown ShutdownConfig
}

type JSONRPCConfig struct {
	Enable             bool
	API                []string
	Address            string
	WsAddress          string
	EnableUnsafeCors   bool
	GasCap             uint64
	EVMTimeout         time.Duration
	TxFeeCap           float64
	FilterCap          int32
	FeeHistoryCap      int32
	LogsCap            int32
	BlockRangeCap      int32
	HTTPTimeout        time.Duration
	HTTPIdleTimeout    time.Duration
	AllowUnprotectedTx bool
	MaxOpenConnections int
	ReturnDataLimit    int64
}

type TracingConfig struct {
	Enabled                     bool
	CollectorDSN                string
	CollectorAuthorization      string
	CollectorAuthorizationField string
	CollectorEnableTLS          bool
	ClusterID                   string
}

type ShutdownConfig struct {
	Timeout time.Duration
}

func DefaultConfig() Config {
	return Config{
		Env:        "local",
		LogFormat:  "json",
		LogVerbose: false,

		ChainID:                "",
		EVMChainID:             "",
		CometRPC:               "http://localhost:26657",
		GRPCAddr:               "localhost:9090",
		Earliest:               1,
		FetchJobs:              4,
		DataDir:                "~/.evm-gateway",
		DBBackend:              "goleveldb",
		AllowGaps:              true,
		EnableSync:             true,
		ParallelSyncTipAndGaps: true,
		OfflineRPCOnly:         false,
		MinGasPrices:           "160000000inj",

		JSONRPC: JSONRPCConfig{
			Enable:             true,
			API:                []string{"eth", "net", "web3"},
			Address:            "0.0.0.0:8545",
			WsAddress:          "0.0.0.0:8546",
			EnableUnsafeCors:   false,
			GasCap:             25000000,
			EVMTimeout:         5 * time.Second,
			TxFeeCap:           1.0,
			FilterCap:          200,
			FeeHistoryCap:      100,
			LogsCap:            10000,
			BlockRangeCap:      10000,
			HTTPTimeout:        30 * time.Second,
			HTTPIdleTimeout:    120 * time.Second,
			AllowUnprotectedTx: false,
			MaxOpenConnections: 0,
			ReturnDataLimit:    512000,
		},
		Tracing: TracingConfig{
			Enabled:            false,
			CollectorEnableTLS: true,
			ClusterID:          "inj",
		},
		Shutdown: ShutdownConfig{
			Timeout: 10 * time.Second,
		},
	}
}

// Load reads a .env-style file (if provided) and overlays environment variables onto defaults.
func Load(envFile string) (Config, error) {
	cfg := DefaultConfig()

	if envFile != "" {
		if err := loadEnvFile(envFile); err != nil {
			return cfg, err
		}
	} else {
		_ = loadEnvFileIfExists(".env")
	}

	applyEnvOverrides(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func (c Config) Validate() error {
	if c.FetchJobs <= 0 {
		return errors.New("fetch-jobs must be greater than 0")
	}
	if c.Earliest < 0 {
		return errors.New("earliest block cannot be negative")
	}
	if c.JSONRPC.Enable && len(c.JSONRPC.API) == 0 {
		return errors.New("jsonrpc api list cannot be empty when enabled")
	}
	if c.JSONRPC.FilterCap < 0 || c.JSONRPC.FeeHistoryCap <= 0 {
		return errors.New("jsonrpc filter-cap or feehistory-cap invalid")
	}
	if c.JSONRPC.TxFeeCap < 0 {
		return errors.New("jsonrpc txfee-cap cannot be negative")
	}
	if c.JSONRPC.LogsCap < 0 || c.JSONRPC.BlockRangeCap < 0 {
		return errors.New("jsonrpc logs-cap or block-range-cap invalid")
	}
	if c.JSONRPC.HTTPTimeout < 0 || c.JSONRPC.HTTPIdleTimeout < 0 {
		return errors.New("jsonrpc http timeouts cannot be negative")
	}
	if c.Shutdown.Timeout <= 0 {
		return errors.New("shutdown timeout must be positive")
	}
	if strings.TrimSpace(c.EVMChainID) != "" {
		evmChainID, ok := new(big.Int).SetString(strings.TrimSpace(c.EVMChainID), 10)
		if !ok || evmChainID.Sign() <= 0 {
			return errors.New("evm-chain-id must be a positive base-10 integer")
		}
	}
	if c.OfflineRPCOnly {
		if c.EnableSync {
			return errors.New("offline-rpc-only mode requires enable-sync=false")
		}
		if strings.TrimSpace(c.ChainID) == "" {
			return errors.New("offline-rpc-only mode requires chain-id")
		}
	}
	return nil
}

// GetMinGasPrices parses the configured minimum gas prices.
func (c Config) GetMinGasPrices() sdk.DecCoins {
	if c.MinGasPrices == "" {
		return sdk.DecCoins{}
	}
	prices, err := sdk.ParseDecCoins(c.MinGasPrices)
	if err != nil {
		panic(fmt.Sprintf("invalid minimum gas prices: %v", err))
	}
	return prices
}

// SetMinGasPrices overrides the configured minimum gas prices.
func (c *Config) SetMinGasPrices(prices sdk.DecCoins) {
	c.MinGasPrices = prices.String()
}

func loadEnvFileIfExists(path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return loadEnvFile(path)
}

func loadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return errors.Wrap(err, "read env file")
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"`)

		if key == "" {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return errors.Wrapf(err, "set env %s", key)
		}
	}

	if err := scanner.Err(); err != nil {
		return errors.Wrap(err, "scan env file")
	}
	return nil
}

func applyEnvOverrides(cfg *Config) {
	cfg.Env = getEnvString("ENV", cfg.Env)
	cfg.LogFormat = getEnvString("LOG_FORMAT", cfg.LogFormat)
	cfg.LogVerbose = getEnvBool("LOG_VERBOSE", cfg.LogVerbose)

	cfg.ChainID = getEnvString("CHAIN_ID", cfg.ChainID)
	cfg.EVMChainID = getEnvString("EVM_CHAIN_ID", cfg.EVMChainID)
	cfg.CometRPC = getEnvString("COMET_RPC", cfg.CometRPC)
	cfg.GRPCAddr = getEnvString("GRPC_ADDR", cfg.GRPCAddr)
	cfg.Earliest = getEnvInt64("EARLIEST_BLOCK", cfg.Earliest)
	cfg.FetchJobs = getEnvInt("FETCH_JOBS", cfg.FetchJobs)
	cfg.DataDir = getEnvString("DATA_DIR", cfg.DataDir)
	cfg.DBBackend = getEnvString("DB_BACKEND", cfg.DBBackend)
	cfg.AllowGaps = getEnvBool("ALLOW_GAPS", cfg.AllowGaps)
	cfg.EnableSync = getEnvBool("ENABLE_SYNC", cfg.EnableSync)
	cfg.ParallelSyncTipAndGaps = getEnvBool("PARALLEL_SYNC_TIP_AND_GAPS", cfg.ParallelSyncTipAndGaps)
	cfg.OfflineRPCOnly = getEnvBool("OFFLINE_RPC_ONLY", cfg.OfflineRPCOnly)
	cfg.MinGasPrices = getEnvString("MIN_GAS_PRICES", cfg.MinGasPrices)
	cfg.Shutdown.Timeout = getEnvDuration("SHUTDOWN_TIMEOUT", cfg.Shutdown.Timeout)

	cfg.JSONRPC.Enable = getEnvBool("JSONRPC_ENABLE", cfg.JSONRPC.Enable)
	cfg.JSONRPC.API = getEnvCSV("JSONRPC_API", cfg.JSONRPC.API)
	cfg.JSONRPC.Address = getEnvString("JSONRPC_ADDRESS", cfg.JSONRPC.Address)
	cfg.JSONRPC.WsAddress = getEnvString("JSONRPC_WS_ADDRESS", cfg.JSONRPC.WsAddress)
	cfg.JSONRPC.EnableUnsafeCors = getEnvBool("JSONRPC_ENABLE_UNSAFE_CORS", cfg.JSONRPC.EnableUnsafeCors)
	cfg.JSONRPC.GasCap = uint64(getEnvInt64("JSONRPC_GAS_CAP", int64(cfg.JSONRPC.GasCap)))
	cfg.JSONRPC.EVMTimeout = getEnvDuration("JSONRPC_EVM_TIMEOUT", cfg.JSONRPC.EVMTimeout)
	cfg.JSONRPC.TxFeeCap = getEnvFloat("JSONRPC_TXFEE_CAP", cfg.JSONRPC.TxFeeCap)
	cfg.JSONRPC.FilterCap = int32(getEnvInt("JSONRPC_FILTER_CAP", int(cfg.JSONRPC.FilterCap)))
	cfg.JSONRPC.FeeHistoryCap = int32(getEnvInt("JSONRPC_FEEHISTORY_CAP", int(cfg.JSONRPC.FeeHistoryCap)))
	cfg.JSONRPC.LogsCap = int32(getEnvInt("JSONRPC_LOGS_CAP", int(cfg.JSONRPC.LogsCap)))
	cfg.JSONRPC.BlockRangeCap = int32(getEnvInt("JSONRPC_BLOCK_RANGE_CAP", int(cfg.JSONRPC.BlockRangeCap)))
	cfg.JSONRPC.HTTPTimeout = getEnvDuration("JSONRPC_HTTP_TIMEOUT", cfg.JSONRPC.HTTPTimeout)
	cfg.JSONRPC.HTTPIdleTimeout = getEnvDuration("JSONRPC_HTTP_IDLE_TIMEOUT", cfg.JSONRPC.HTTPIdleTimeout)
	cfg.JSONRPC.AllowUnprotectedTx = getEnvBool("JSONRPC_ALLOW_UNPROTECTED_TXS", cfg.JSONRPC.AllowUnprotectedTx)
	cfg.JSONRPC.MaxOpenConnections = getEnvInt("JSONRPC_MAX_OPEN_CONNECTIONS", cfg.JSONRPC.MaxOpenConnections)
	cfg.JSONRPC.ReturnDataLimit = getEnvInt64("JSONRPC_RETURN_DATA_LIMIT", cfg.JSONRPC.ReturnDataLimit)

	cfg.Tracing.Enabled = getEnvBool("GOTRACER_ENABLED", cfg.Tracing.Enabled)
	cfg.Tracing.CollectorDSN = getEnvString("GOTRACER_COLLECTOR_DSN", cfg.Tracing.CollectorDSN)
	cfg.Tracing.CollectorAuthorization = getEnvString("GOTRACER_COLLECTOR_AUTHORIZATION", cfg.Tracing.CollectorAuthorization)
	cfg.Tracing.CollectorAuthorizationField = getEnvString("GOTRACER_COLLECTOR_AUTHORIZATION_HEADER", cfg.Tracing.CollectorAuthorizationField)
	cfg.Tracing.CollectorEnableTLS = getEnvBool("GOTRACER_COLLECTOR_ENABLE_TLS", cfg.Tracing.CollectorEnableTLS)
	cfg.Tracing.ClusterID = getEnvString("GOTRACER_CLUSTER_ID", cfg.Tracing.ClusterID)
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	return path
}

func getEnvString(key string, fallback string) string {
	if value := os.Getenv(envPrefix + key); value != "" {
		return value
	}
	return fallback
}

func getEnvCSV(key string, fallback []string) []string {
	value := os.Getenv(envPrefix + key)
	if value == "" {
		return fallback
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

func getEnvBool(key string, fallback bool) bool {
	value := strings.ToLower(os.Getenv(envPrefix + key))
	switch value {
	case "true", "1", "t", "yes", "y":
		return true
	case "false", "0", "f", "no", "n":
		return false
	default:
		return fallback
	}
}

func getEnvInt(key string, fallback int) int {
	value := os.Getenv(envPrefix + key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getEnvInt64(key string, fallback int64) int64 {
	value := os.Getenv(envPrefix + key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func getEnvFloat(key string, fallback float64) float64 {
	value := os.Getenv(envPrefix + key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(envPrefix + key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// Expand resolves any path fields that support the ~ prefix.
func (c *Config) Expand() {
	c.DataDir = expandPath(c.DataDir)
}
