package indexer

import (
	"fmt"

	"github.com/pkg/errors"
)

var ErrBlockParse = errors.New("block parse error")
var ErrCacheMiss = errors.New("cache miss")

type blockParseError struct {
	msg string
	err error
}

func (e *blockParseError) Error() string {
	if e.err == nil {
		return e.msg
	}
	return fmt.Sprintf("%s: %v", e.msg, e.err)
}

func (e *blockParseError) Unwrap() error {
	return e.err
}

func (e *blockParseError) Is(target error) bool {
	return target == ErrBlockParse || errors.Is(e.err, target)
}

func newBlockParseError(err error, format string, args ...interface{}) error {
	return &blockParseError{
		msg: fmt.Sprintf(format, args...),
		err: err,
	}
}

func newCacheMiss(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrCacheMiss, fmt.Sprintf(format, args...))
}
