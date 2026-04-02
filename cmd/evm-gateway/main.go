package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/pflag"

	"github.com/InjectiveLabs/sdk-go/chain/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	dbm "github.com/cosmos/cosmos-db"

	"github.com/InjectiveLabs/evm-gateway/internal/app"
	"github.com/InjectiveLabs/evm-gateway/internal/config"
	txindexer "github.com/InjectiveLabs/evm-gateway/internal/indexer"
	"github.com/InjectiveLabs/evm-gateway/internal/logging"
	"github.com/InjectiveLabs/evm-gateway/internal/telemetry"
	"github.com/InjectiveLabs/evm-gateway/version"
)

type flagOverrides struct {
	envFile           string
	printVersion      bool
	chainID           string
	cometRPC          string
	grpcAddr          string
	earliest          int64
	fetchJobs         int
	dataDir           string
	logFormat         string
	logVerbose        bool
	logVerboseSet     bool
	rpcAddr           string
	wsAddr            string
	apiList           []string
	apiListSet        bool
	statsdEnabled     bool
	statsdEnabledSet  bool
	statsdAddr        string
	statsdPrefix      string
	tracingEnabled    bool
	tracingEnabledSet bool
	tracingDSN        string
}

func main() {
	// Dispatch subcommands before the normal flag parse so that "reindex"
	// gets its own isolated flag set.
	if len(os.Args) > 1 && os.Args[1] == "reindex" {
		runReindex(os.Args[2:])
		return
	}

	flags := parseFlags()
	if flags.printVersion {
		fmt.Println(version.Version())
		return
	}

	cfg, err := config.Load(flags.envFile)
	if err != nil {
		fail(err)
	}

	applyOverrides(&cfg, flags)
	cfg.Expand()

	logger := logging.New(logging.Config{
		Format:  cfg.LogFormat,
		Verbose: cfg.LogVerbose,
		Output:  os.Stdout,
	})

	sdkConfig := sdk.GetConfig()
	types.SetBech32Prefixes(sdkConfig)
	sdkConfig.Seal()

	statsdClient, err := telemetry.InitStatsd(cfg.Statsd, cfg.Env, logger)
	if err != nil {
		logger.Error("statsd init failed", "error", err)
	}
	telemetry.InitTracing(cfg.Tracing, cfg.Env, logger)

	if err := app.Run(cfg, logger, statsdClient); err != nil {
		logger.Error("evm-gateway failed", "error", err)
		os.Exit(1)
	}
}

func parseFlags() flagOverrides {
	var flags flagOverrides
	pflag.StringVar(&flags.envFile, "env-file", "", "Path to .env file with WEB3INJ_ variables")
	pflag.BoolVar(&flags.printVersion, "version", false, "Print build version information and exit")
	pflag.StringVar(&flags.chainID, "chain-id", "", "Expected chain ID")
	pflag.StringVar(&flags.cometRPC, "comet-rpc", "", "CometBFT RPC endpoint")
	pflag.StringVar(&flags.grpcAddr, "grpc-addr", "", "gRPC endpoint")
	pflag.Int64Var(&flags.earliest, "earliest-block", 0, "Earliest block height to index from")
	pflag.IntVar(&flags.fetchJobs, "fetch-jobs", 0, "Parallel block fetch jobs")
	pflag.StringVar(&flags.dataDir, "data-dir", "", "Data directory for indexer DB")
	pflag.StringVar(&flags.logFormat, "log-format", "", "Log format: json or text")
	pflag.BoolVar(&flags.logVerbose, "log-verbose", false, "Enable verbose logging")
	pflag.StringVar(&flags.rpcAddr, "rpc-address", "", "JSON-RPC HTTP listen address")
	pflag.StringVar(&flags.wsAddr, "ws-address", "", "JSON-RPC WS listen address")
	pflag.StringSliceVar(&flags.apiList, "rpc-api", nil, "Comma-separated JSON-RPC API namespaces")
	pflag.BoolVar(&flags.statsdEnabled, "statsd-enabled", false, "Enable statsd")
	pflag.StringVar(&flags.statsdAddr, "statsd-addr", "", "Statsd address")
	pflag.StringVar(&flags.statsdPrefix, "statsd-prefix", "", "Statsd prefix")
	pflag.BoolVar(&flags.tracingEnabled, "gotracer-enabled", false, "Enable gotracer")
	pflag.StringVar(&flags.tracingDSN, "gotracer-collector-dsn", "", "Otel collector DSN")
	pflag.Parse()

	flags.logVerboseSet = pflag.CommandLine.Changed("log-verbose")
	flags.apiListSet = pflag.CommandLine.Changed("rpc-api")
	flags.statsdEnabledSet = pflag.CommandLine.Changed("statsd-enabled")
	flags.tracingEnabledSet = pflag.CommandLine.Changed("gotracer-enabled")

	return flags
}

func applyOverrides(cfg *config.Config, flags flagOverrides) {
	if flags.chainID != "" {
		cfg.ChainID = flags.chainID
	}
	if flags.cometRPC != "" {
		cfg.CometRPC = flags.cometRPC
	}
	if flags.grpcAddr != "" {
		cfg.GRPCAddr = flags.grpcAddr
	}
	if flags.earliest > 0 {
		cfg.Earliest = flags.earliest
	}
	if flags.fetchJobs > 0 {
		cfg.FetchJobs = flags.fetchJobs
	}
	if flags.dataDir != "" {
		cfg.DataDir = flags.dataDir
	}
	if flags.logFormat != "" {
		cfg.LogFormat = flags.logFormat
	}
	if flags.logVerboseSet {
		cfg.LogVerbose = flags.logVerbose
	}
	if flags.rpcAddr != "" {
		cfg.JSONRPC.Address = flags.rpcAddr
	}
	if flags.wsAddr != "" {
		cfg.JSONRPC.WsAddress = flags.wsAddr
	}
	if flags.apiListSet {
		cfg.JSONRPC.API = flags.apiList
	}
	if flags.statsdEnabledSet {
		cfg.Statsd.Enabled = flags.statsdEnabled
	}
	if flags.statsdAddr != "" {
		cfg.Statsd.Addr = flags.statsdAddr
	}
	if flags.statsdPrefix != "" {
		cfg.Statsd.Prefix = flags.statsdPrefix
	}
	if flags.tracingEnabledSet {
		cfg.Tracing.Enabled = flags.tracingEnabled
	}
	if flags.tracingDSN != "" {
		cfg.Tracing.CollectorDSN = flags.tracingDSN
	}
}

func fail(err error) {
	_, _ = os.Stderr.WriteString(err.Error() + "\n")
	os.Exit(1)
}

// runReindex implements the "reindex" subcommand. It deletes all indexed data
// for the given block range so that the syncer re-processes those blocks on
// the next startup. The gateway must NOT be running while this command executes.
//
// Usage:
//
//	evm-gateway reindex --from <height> --to <height> [--data-dir <path>] [--db-backend <backend>]
func runReindex(args []string) {
	fs := pflag.NewFlagSet("reindex", pflag.ContinueOnError)

	var (
		from      int64
		to        int64
		envFile   string
		dataDir   string
		dbBackend string
		logFormat string
	)
	fs.Int64Var(&from, "from", 0, "First block height to clear (inclusive, required)")
	fs.Int64Var(&to, "to", 0, "Last block height to clear (inclusive, required)")
	fs.StringVar(&envFile, "env-file", "", "Path to .env file with WEB3INJ_ variables")
	fs.StringVar(&dataDir, "data-dir", "", "Data directory for indexer DB (overrides env/config)")
	fs.StringVar(&dbBackend, "db-backend", "", "DB backend type (overrides env/config)")
	fs.StringVar(&logFormat, "log-format", "", "Log format: json or text")

	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "reindex: %v\n\nUsage:\n", err)
		fs.PrintDefaults()
		os.Exit(1)
	}

	if from <= 0 || to <= 0 {
		_, _ = fmt.Fprintln(os.Stderr, "reindex: --from and --to are required and must be > 0")
		fs.PrintDefaults()
		os.Exit(1)
	}
	if from > to {
		_, _ = fmt.Fprintf(os.Stderr, "reindex: --from (%d) must be <= --to (%d)\n", from, to)
		os.Exit(1)
	}

	cfg, err := config.Load(envFile)
	if err != nil {
		fail(err)
	}
	if dataDir != "" {
		cfg.DataDir = dataDir
	}
	if dbBackend != "" {
		cfg.DBBackend = dbBackend
	}
	if logFormat != "" {
		cfg.LogFormat = logFormat
	}
	cfg.Expand()

	logger := logging.New(logging.Config{
		Format:  cfg.LogFormat,
		Verbose: cfg.LogVerbose,
		Output:  os.Stdout,
	})

	dbPath := filepath.Join(cfg.DataDir, "data")
	db, err := dbm.NewDB("evmindexer", dbm.BackendType(cfg.DBBackend), dbPath)
	if err != nil {
		fail(fmt.Errorf("open indexer DB at %s: %w", dbPath, err))
	}
	defer db.Close()

	if err := txindexer.ClearBlockRange(db, logger, from, to); err != nil {
		fail(err)
	}
}
