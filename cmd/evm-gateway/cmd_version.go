package main

import (
	"fmt"
	"io"
	"os"

	cli "github.com/jawher/mow.cli"

	"github.com/InjectiveLabs/evm-gateway/version"
)

func versionCmd(cmd *cli.Cmd) {
	cmd.Action = func() {
		printVersion(os.Stdout)
	}
}

func printVersion(w io.Writer) {
	_, _ = fmt.Fprintln(w, version.Version())
}
