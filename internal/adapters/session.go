package adapters

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/trevorfox/gtm/internal/protocol"
)

// ExitError carries an external adapter's exit status, so the gtm process can
// exit with the same class of code the adapter used (SPEC §8: 3 auth,
// 4 rate-limited, 5 network).
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode reports the adapter's exit code when it is one gtm defines, else 0 so
// the caller falls back to its own default.
func (e *ExitError) ExitCode() int {
	switch e.Code {
	case 2, 3, 4, 5:
		return e.Code
	default:
		return 0
	}
}

// Ports is everything an adapter gets: the runner's message stream in, its own
// message stream out, a place for human-facing noise, and the credentials the
// manifest declared.
type Ports struct {
	In   io.Reader
	Out  io.Writer
	Log  io.Writer // stderr equivalent; never data
	Env  map[string]string
	Args []string // external adapters only
}

// Getenv reads a declared credential.
func (p Ports) Getenv(key string) string { return p.Env[key] }

// Adapter is a built-in adapter: a process-shaped thing that happens to run in
// this address space. It reads runner→adapter NDJSON from Ports.In and writes
// adapter→runner NDJSON to Ports.Out, exactly as an external executable would.
type Adapter interface {
	Run(ctx context.Context, p Ports) error
}

// Session is one adapter invocation: OPEN, records, END, exit.
type Session struct {
	w       *protocol.Writer
	r       *protocol.Reader
	closeIn func() error
	wait    func() error

	closeOnce sync.Once
	waitOnce  sync.Once
	waitErr   error
}

// Send writes one runner→adapter message.
func (s *Session) Send(m protocol.Message) error { return s.w.Write(m) }

// Next reads the next adapter→runner message; io.EOF ends the stream.
func (s *Session) Next() (protocol.Message, error) { return s.r.Next() }

// CloseSend closes the adapter's stdin, which is how END is really final for
// adapters that read until EOF.
func (s *Session) CloseSend() error {
	var err error
	s.closeOnce.Do(func() { err = s.closeIn() })
	return err
}

// SendStream writes every message in the background and then closes the
// adapter's stdin, returning a channel that yields the write error (nil on
// success).
//
// This has to be concurrent with reading: the transport has no buffer, so an
// adapter that starts replying before it has read all its input would deadlock
// against a runner that writes all its input before reading.
func (s *Session) SendStream(msgs []protocol.Message) <-chan error {
	done := make(chan error, 1)
	go func() {
		for _, m := range msgs {
			if err := s.Send(m); err != nil {
				s.CloseSend()
				done <- err
				return
			}
		}
		done <- s.CloseSend()
	}()
	return done
}

// Wait blocks until the adapter finishes and reports its fatal error, if any.
func (s *Session) Wait() error {
	s.waitOnce.Do(func() { s.waitErr = s.wait() })
	return s.waitErr
}

// launchBuiltin runs a compiled-in adapter over a pair of pipes. The transport
// differs from an external adapter; the protocol does not.
func launchBuiltin(ctx context.Context, a Adapter, p Ports) *Session {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	done := make(chan error, 1)
	go func() {
		err := a.Run(ctx, Ports{In: inR, Out: outW, Log: p.Log, Env: p.Env})
		// Closing with the error surfaces it to the reader too, so a runner that
		// is still reading does not hang.
		outW.CloseWithError(err)
		inR.CloseWithError(err)
		done <- err
	}()

	return &Session{
		w:       protocol.NewWriter(inW),
		r:       protocol.NewReader(outR),
		closeIn: inW.Close,
		wait: func() error {
			err := <-done
			outR.Close()
			return err
		},
	}
}

// launchExec runs an external adapter executable.
func launchExec(ctx context.Context, dir, executable string, p Ports) (*Session, error) {
	cmd := exec.CommandContext(ctx, executable, p.Args...)
	cmd.Dir = dir

	// A deliberately small environment: the process basics plus exactly the
	// credentials the manifest declared (SPEC §6).
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LANG=" + os.Getenv("LANG"),
		"PYTHONUNBUFFERED=1",
	}
	for k, v := range p.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("adapters: %s: stdin: %w", executable, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("adapters: %s: stdout: %w", executable, err)
	}
	cmd.Stderr = prefixWriter{w: p.Log, prefix: filepath.Base(filepath.Dir(executable)) + ": "}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("adapters: starting %s: %w", executable, err)
	}

	return &Session{
		w:       protocol.NewWriter(stdin),
		r:       protocol.NewReader(stdout),
		closeIn: stdin.Close,
		wait: func() error {
			if err := cmd.Wait(); err != nil {
				code := 0
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					code = ee.ExitCode()
				}
				return &ExitError{
					Code: code,
					Err:  fmt.Errorf("adapters: %s: %w", executable, err),
				}
			}
			return nil
		},
	}, nil
}

// prefixWriter tags an adapter's stderr so it is obvious which adapter spoke.
type prefixWriter struct {
	w      io.Writer
	prefix string
}

func (p prefixWriter) Write(b []byte) (int, error) {
	if p.w == nil {
		return len(b), nil
	}
	for _, line := range splitLines(b) {
		if line == "" {
			continue
		}
		if _, err := io.WriteString(p.w, p.prefix+line+"\n"); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

func splitLines(b []byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			out = append(out, string(b[start:i]))
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}
