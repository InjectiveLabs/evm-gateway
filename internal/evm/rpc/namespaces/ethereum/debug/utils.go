package debug

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime/pprof"
	"strings"

	"log/slog"
)

// isCPUProfileConfigurationActivated checks if cpuprofile was configured via flag
func isCPUProfileConfigurationActivated() bool {
	return false
}

// ExpandHome expands home directory in file paths.
// ~someuser/tmp will not be expanded.
func ExpandHome(p string) (string, error) {
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		usr, err := user.Current()
		if err != nil {
			return p, err
		}
		home := usr.HomeDir
		p = home + p[1:]
	}
	return filepath.Clean(p), nil
}

// writeProfile writes the data to a file
func writeProfile(name, file string, logger *slog.Logger) error {
	p := pprof.Lookup(name)
	logger.Info("Writing profile records", "count", p.Count(), "type", name, "dump", file)
	fp, err := ExpandHome(file)
	if err != nil {
		return err
	}
	f, err := os.Create(fp)
	if err != nil {
		return err
	}

	if err := p.WriteTo(f, 0); err != nil {
		if err := f.Close(); err != nil {
			return err
		}
		return err
	}

	return f.Close()
}
