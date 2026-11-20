package main

import (
	"os"
	"strings"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
)

// envFileFlag is the global option naming the env file to load.
const envFileFlag = "env-file"

// loadEnv loads WEB3INJ_ variables into the process environment before the CLI
// is built: options take their defaults from the environment when they are
// declared, which happens before arguments are parsed. The file named by
// --env-file is loaded if given, otherwise `.env` in the working directory if
// present.
func loadEnv(args []string) error {
	return config.LoadEnvFile(envFileFromArgs(args))
}

// envFileFromArgs returns the value of the --env-file option, scanning
// arguments up to "--". Without the CLI spec, option values can't be told
// apart from commands, so every argument is scanned; no command defines an
// option of the same name, and a misplaced one is still rejected by the CLI.
func envFileFromArgs(args []string) string {
	if len(args) > 0 {
		args = args[1:]
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return ""
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name != envFileFlag {
			continue
		}
		if hasValue {
			return value
		}
		if i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	return ""
}

func parseCSV(value string, fallback []string) []string {
	if value == "" {
		return fallback
	}

	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}

	if len(out) == 0 {
		return fallback
	}

	return out
}

func fail(err error) {
	_, _ = os.Stderr.WriteString(err.Error() + "\n")
	os.Exit(1)
}
