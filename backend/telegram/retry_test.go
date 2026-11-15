package telegram

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/pool"
	"github.com/gotd/td/tgerr"
)

type invokerFunc func() error

func (f invokerFunc) Invoke(context.Context, bin.Encoder, bin.Decoder) error { return f() }

func TestRetryMiddleware(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		calls int
	}{
		{"unexpected EOF", io.ErrUnexpectedEOF, 2},
		{"wrapped conn dead", fmt.Errorf("invoke: %w", pool.ErrConnDead), 2},
		{"server error", tgerr.New(500, "INTERNAL"), 2},
		{"bad request", tgerr.New(400, "PEER_ID_INVALID"), 1},
		{"canceled", context.Canceled, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			next := invokerFunc(func() error {
				calls++
				if calls == 1 {
					return tc.err
				}
				return nil
			})
			_ = retryMiddleware(5).Handle(next).Invoke(context.Background(), nil, nil)
			if calls != tc.calls {
				t.Errorf("calls = %d, want %d", calls, tc.calls)
			}
		})
	}
}
