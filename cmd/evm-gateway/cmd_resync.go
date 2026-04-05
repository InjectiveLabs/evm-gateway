package main

import "errors"

var resyncRunner = runResync

func runResync(*gatewayCLIOptions) error {
	return errors.New("resync action is not implemented yet")
}
