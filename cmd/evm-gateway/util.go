package main

import (
	"bufio"
	"bytes"
	"log/slog"
	"os"
	"strings"
)

// readEnv is a special utility that reads `.env` file into actual environment variables
// of the current app, similar to `dotenv` Node package.
func readEnv() {
	if envdata, _ := os.ReadFile(".env"); len(envdata) > 0 {
		s := bufio.NewScanner(bytes.NewReader(envdata))
		for s.Scan() {
			txt := s.Text()
			valIdx := strings.IndexByte(txt, '=')
			if valIdx < 0 {
				continue
			}

			strValue := strings.Trim(txt[valIdx+1:], `"`)
			if err := os.Setenv(txt[:valIdx], strValue); err != nil {
				slog.With("name", txt[:valIdx], "error", err).Warn("failed to override EVN variable")
			}
		}
	}
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
