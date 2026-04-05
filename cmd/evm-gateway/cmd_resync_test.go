package main

import (
	"reflect"
	"testing"

	"github.com/InjectiveLabs/evm-gateway/internal/indexer"
)

func TestParseResyncTargetsNormalizesRanges(t *testing.T) {
	targets, err := parseResyncTargets([]string{
		"13000001:13000060",
		"12000000",
		"13000001",
		"12000001:12000005",
	})
	if err != nil {
		t.Fatalf("parseResyncTargets returned error: %v", err)
	}

	want := []indexer.BlockRange{
		{Start: 12000000, End: 12000005},
		{Start: 13000001, End: 13000060},
	}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("unexpected targets: got %#v want %#v", targets, want)
	}

	if got, wantCount := indexer.CountBlocks(targets), int64(66); got != wantCount {
		t.Fatalf("unexpected unique block count: got %d want %d", got, wantCount)
	}
}

func TestParseResyncTargetsRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "no targets", args: nil},
		{name: "negative block", args: []string{"-1"}},
		{name: "invalid block", args: []string{"abc"}},
		{name: "invalid range", args: []string{"10:5"}},
		{name: "too many separators", args: []string{"1:2:3"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseResyncTargets(tc.args); err == nil {
				t.Fatalf("expected parseResyncTargets to fail for %v", tc.args)
			}
		})
	}
}
