// Package collect gathers raw process and system measurements. Platform
// specific code lives in *_darwin.go / *_other.go files; the parsers are
// platform neutral so they can be tested anywhere with fixtures.
package collect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner runs a command and returns its standard output. Implementations
// must honour ctx and must return any stdout captured even when err != nil.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner runs real subprocesses with a timeout and bounded output.
type ExecRunner struct {
	Timeout   time.Duration
	MaxOutput int
}

// Run executes name with args under the C locale so numeric output is stable.
func (r ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	limit := r.MaxOutput
	if limit <= 0 {
		limit = 16 << 20
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	stdout := &limitedBuffer{max: limit}
	stderr := &limitedBuffer{max: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second

	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return stdout.Bytes(), fmt.Errorf("%s timed out after %s", name, timeout)
	}
	if stdout.truncated {
		return stdout.Bytes(), fmt.Errorf("%s output exceeded %d bytes", name, limit)
	}
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("%s failed: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// limitedBuffer keeps at most max bytes but always reports a full write so a
// chatty child is never blocked on a full pipe. The buffer is a named field,
// not embedded: an embedded bytes.Buffer would promote ReadFrom, which
// io.Copy prefers, silently bypassing the limit.
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := b.max - b.buf.Len()
	if room <= 0 {
		b.truncated = b.truncated || len(p) > 0
		return len(p), nil
	}
	if len(p) > room {
		b.buf.Write(p[:room])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *limitedBuffer) String() string { return b.buf.String() }
