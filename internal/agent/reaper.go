package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// reaper owns wait(2) for every child of the agent. The agent runs as PID 1
// in the engine container, so orphaned grandchildren (old nginx workers,
// haproxy processes) are re-parented to it and must be reaped. All child
// processes are therefore started through reaper.start and their exit status
// is delivered on a channel instead of exec.Cmd.Wait.
type reaper struct {
	mu      sync.Mutex
	waiters map[int]chan syscall.WaitStatus
}

func newReaper() *reaper { return &reaper{waiters: map[int]chan syscall.WaitStatus{}} }

func (r *reaper) loop(ctx context.Context) {
	sig := make(chan os.Signal, 16)
	signal.Notify(sig, syscall.SIGCHLD)
	defer signal.Stop(sig)
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sig:
		case <-t.C:
		}
		r.reap()
	}
}

func (r *reaper) reap() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || pid <= 0 {
			return
		}
		if ch, ok := r.waiters[pid]; ok {
			ch <- ws
			delete(r.waiters, pid)
		}
	}
}

// start starts cmd and returns a channel that receives its exit status.
func (r *reaper) start(cmd *exec.Cmd) (<-chan syscall.WaitStatus, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := make(chan syscall.WaitStatus, 1)
	r.waiters[cmd.Process.Pid] = ch
	return ch, nil
}

// run executes a short-lived command and returns its combined output.
func (r *reaper) run(timeout time.Duration, name string, args ...string) (string, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout = pw
	cmd.Stderr = pw
	ch, err := r.start(cmd)
	pw.Close()
	if err != nil {
		pr.Close()
		return "", err
	}
	pid := cmd.Process.Pid
	outc := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(pr, 1<<20))
		pr.Close()
		outc <- b
	}()
	var ws syscall.WaitStatus
	timedOut := false
	select {
	case ws = <-ch:
	case <-time.After(timeout):
		timedOut = true
		syscall.Kill(-pid, syscall.SIGKILL)
		syscall.Kill(pid, syscall.SIGKILL)
		ws = <-ch
	}
	var out []byte
	select {
	case out = <-outc:
	case <-time.After(2 * time.Second):
		pr.Close()
	}
	cmd.Process.Release()
	text := strings.TrimRight(string(out), "\n")
	if timedOut {
		return text, fmt.Errorf("%s timed out after %s", name, timeout)
	}
	if err := exitError(ws); err != nil {
		return text, err
	}
	return text, nil
}

func exitError(ws syscall.WaitStatus) error {
	switch {
	case ws.Exited() && ws.ExitStatus() == 0:
		return nil
	case ws.Exited():
		return fmt.Errorf("exit status %d", ws.ExitStatus())
	case ws.Signaled():
		return fmt.Errorf("killed by signal %s", ws.Signal())
	}
	return fmt.Errorf("exited (status %d)", int(ws))
}
