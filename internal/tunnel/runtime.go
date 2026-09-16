package tunnel

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runtimeSocket answers one command line per connection, like HAProxy's
// runtime API in non-interactive mode: the client writes a line and reads
// the answer until EOF.
type runtimeSocket struct {
	path string
	ln   net.Listener
	e    *Engine
}

func listenRuntime(path string, e *Engine) (*runtimeSocket, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		os.Remove(path) // stale socket of a previous process
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(true)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	return &runtimeSocket{path: path, ln: ln, e: e}, nil
}

func (rs *runtimeSocket) serve() {
	for {
		c, err := rs.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		go rs.handle(c)
	}
}

func (rs *runtimeSocket) handle(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReaderSize(io.LimitReader(c, 4096), 4096).ReadString('\n')
	if err != nil && line == "" {
		return
	}
	io.WriteString(c, rs.e.Command(line))
}

func (rs *runtimeSocket) close() { rs.ln.Close() }

// Command runs one runtime socket command line and returns the answer.
func (e *Engine) Command(line string) string {
	switch cmd := strings.TrimSpace(line); cmd {
	case "status":
		b, err := json.Marshal(e.Status())
		if err != nil {
			return fmt.Sprintf("error: %v\n", err)
		}
		return string(b) + "\n"
	case "help":
		return "commands:\n  status  : the engine and gateway status as JSON\n"
	default:
		return fmt.Sprintf("error: unknown command %q (try \"help\")\n", cmd)
	}
}
