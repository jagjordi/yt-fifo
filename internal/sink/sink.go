// Package sink opens the place PCM is written to.
//
// Three destinations are supported, chosen by the spec string:
//
//   - stdout, for use as a snapserver process:// source
//     tcp://host:port  a snapserver tcp:// source in server mode
//     /path/to/fifo    a named pipe, for a snapserver pipe:// source
//
// The pipe and TCP sinks reconnect: writes block while nobody is listening
// and resume once a reader appears, so the player never needs to know.
package sink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Open returns a writer for spec. Stdout is returned as-is when spec is "-"
// or empty.
func Open(ctx context.Context, spec string, log *slog.Logger) (io.WriteCloser, error) {
	switch {
	case spec == "" || spec == "-":
		return nopCloser{os.Stdout}, nil
	case strings.HasPrefix(spec, "tcp://"):
		addr := strings.TrimPrefix(spec, "tcp://")
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return nil, fmt.Errorf("bad tcp sink %q: %w", spec, err)
		}
		return &reconnecting{ctx: ctx, log: log, name: spec, dial: func() (io.WriteCloser, error) {
			var d net.Dialer
			c, err := d.DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			return c, nil
		}}, nil
	default:
		if err := ensureFIFO(spec); err != nil {
			return nil, err
		}
		return &reconnecting{ctx: ctx, log: log, name: spec, dial: func() (io.WriteCloser, error) {
			return openFIFO(ctx, spec, log)
		}}, nil
	}
}

// openFIFO opens the write side of a named pipe, blocking until a reader
// exists.
//
// A blocking open is deliberate. snapserver opens its pipe sources
// O_RDONLY|O_NONBLOCK, reads EOF at once while no writer is attached, closes
// the descriptor and tries again 100 ms later, so the read side is open for
// only microseconds per cycle. A non-blocking O_WRONLY open therefore fails
// with ENXIO almost every time; a blocking one is completed by the kernel the
// moment snapserver's next open comes along. The open runs in a goroutine so
// shutdown is not held up by it; if ctx ends first the goroutine closes the
// descriptor whenever the open eventually returns.
func openFIFO(ctx context.Context, path string, log *slog.Logger) (io.WriteCloser, error) {
	type result struct {
		f   *os.File
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		ch <- result{f, err}
	}()
	waited := time.NewTimer(time.Second)
	defer waited.Stop()
	for {
		select {
		case r := <-ch:
			return r.f, r.err
		case <-waited.C:
			log.Info("waiting for a reader to open the pipe", "sink", path)
		case <-ctx.Done():
			go func() {
				if r := <-ch; r.f != nil {
					_ = r.f.Close()
				}
			}()
			return nil, ctx.Err()
		}
	}
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func ensureFIFO(path string) error {
	st, err := os.Stat(path)
	switch {
	case err == nil:
		if st.Mode()&os.ModeNamedPipe == 0 {
			return fmt.Errorf("%s exists but is not a FIFO", path)
		}
		return nil
	case errors.Is(err, os.ErrNotExist):
		if err := syscall.Mkfifo(path, 0o666); err != nil {
			return fmt.Errorf("mkfifo %s: %w", path, err)
		}
		return nil
	default:
		return err
	}
}

// reconnecting wraps a dial function. Write blocks until a connection
// exists and the data went out; a failed write drops the connection, waits
// and tries again with the same buffer, so at most one write's worth of
// audio is lost per disconnect.
type reconnecting struct {
	ctx  context.Context
	log  *slog.Logger
	name string
	dial func() (io.WriteCloser, error)

	mu   sync.Mutex
	conn io.WriteCloser
}

func (r *reconnecting) connect() error {
	delay := 250 * time.Millisecond
	warned := false
	for {
		c, err := r.dial()
		if err == nil {
			if warned {
				r.log.Info("output connected", "sink", r.name)
			}
			r.conn = c
			return nil
		}
		if ctx := r.ctx; ctx.Err() != nil {
			return ctx.Err()
		}
		if !warned {
			r.log.Warn("output not ready, retrying", "sink", r.name, "err", err)
			warned = true
		}
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-time.After(delay):
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

func (r *reconnecting) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		if r.conn == nil {
			if err := r.connect(); err != nil {
				return 0, err
			}
		}
		n, err := r.conn.Write(b)
		if err == nil {
			return n, nil
		}
		r.log.Warn("output write failed, reconnecting", "sink", r.name, "err", err)
		_ = r.conn.Close()
		r.conn = nil
		if r.ctx.Err() != nil {
			return n, r.ctx.Err()
		}
	}
}

func (r *reconnecting) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		err := r.conn.Close()
		r.conn = nil
		return err
	}
	return nil
}
