package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// process is one run of the engine master process.
type process struct {
	pid       int
	startedAt time.Time
	logSeq    uint64 // ring log position when the process started
	done      chan struct{}
	status    syscall.WaitStatus
}

// supervisor runs the engine process, captures its output and restarts it
// with backoff when it dies unexpectedly.
type supervisor struct {
	ctx         context.Context
	log         *slog.Logger
	reaper      *reaper
	logs        *ringLog
	command     func() *exec.Cmd
	stopSignal  syscall.Signal
	beforeStart func()

	// op serialises high-level operations (apply, rollback, start, stop,
	// reload) so the restart timer never races with them.
	op sync.Mutex

	mu       sync.Mutex
	proc     *process
	want     bool
	exitedAt *time.Time
	exitErr  string
	failures int
	timer    *time.Timer
}

func (s *supervisor) current() *process {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc
}

func (s *supervisor) running() bool { return s.current() != nil }

func (s *supervisor) wanted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.want
}

type procState struct {
	pid       int
	startedAt *time.Time
	exitedAt  *time.Time
	exitErr   string
}

func (s *supervisor) state() procState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := procState{exitedAt: s.exitedAt, exitErr: s.exitErr}
	if s.proc != nil {
		t := s.proc.startedAt
		st.pid, st.startedAt = s.proc.pid, &t
		st.exitedAt, st.exitErr = nil, ""
	}
	return st
}

// start launches the engine if it is not running. Callers hold s.op.
func (s *supervisor) start() (*process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.want = true
	if s.proc != nil {
		return s.proc, nil
	}
	return s.startLocked()
}

func (s *supervisor) startLocked() (*process, error) {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if s.beforeStart != nil {
		s.beforeStart()
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd := s.command()
	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p := &process{startedAt: time.Now().UTC(), logSeq: s.logs.mark(), done: make(chan struct{})}
	ch, err := s.reaper.start(cmd)
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		now := time.Now().UTC()
		s.exitedAt, s.exitErr = &now, err.Error()
		s.logs.add("stderr", "relay-agent: start failed: "+err.Error())
		return nil, err
	}
	p.pid = cmd.Process.Pid
	cmd.Process.Release()
	go s.logs.pump(outR, "stdout")
	go s.logs.pump(errR, "stderr")
	s.proc = p
	s.exitErr = ""
	s.log.Info("engine started", "pid", p.pid)
	go s.watch(p, ch)
	return p, nil
}

func (s *supervisor) watch(p *process, ch <-chan syscall.WaitStatus) {
	ws := <-ch
	p.status = ws
	// Give the pumps a moment to flush the last stderr lines.
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	if s.proc == p {
		s.proc = nil
		now := time.Now().UTC()
		s.exitedAt = &now
		// Clean up any workers left in the process group.
		syscall.Kill(-p.pid, syscall.SIGKILL)
		if s.want {
			s.exitErr = describeExit(ws, s.logs.after(p.logSeq))
			s.log.Warn("engine exited unexpectedly", "pid", p.pid, "err", s.exitErr)
			if s.ctx.Err() == nil {
				s.scheduleRestartLocked(now.Sub(p.startedAt))
			}
		} else {
			s.exitErr = ""
			s.log.Info("engine stopped", "pid", p.pid)
		}
	}
	s.mu.Unlock()
	close(p.done)
}

func describeExit(ws syscall.WaitStatus, lines []LogLine) string {
	msg := "exited"
	if err := exitError(ws); err != nil {
		msg = err.Error()
	}
	if errs := errorLines(lines); len(errs) > 0 {
		msg += ": " + errs[len(errs)-1].Text
	}
	return msg
}

func (s *supervisor) scheduleRestartLocked(ran time.Duration) {
	if ran > time.Minute {
		s.failures = 0
	}
	s.failures++
	delay := 5 * time.Second
	for i := 1; i < s.failures && delay < time.Minute; i++ {
		delay *= 2
	}
	if delay > time.Minute {
		delay = time.Minute
	}
	var retry func()
	retry = func() {
		if s.ctx.Err() != nil {
			return
		}
		if !s.op.TryLock() {
			s.mu.Lock()
			s.timer = time.AfterFunc(5*time.Second, retry)
			s.mu.Unlock()
			return
		}
		defer s.op.Unlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.want && s.proc == nil {
			s.log.Info("restarting engine", "attempt", s.failures)
			s.startLocked()
		}
	}
	s.timer = time.AfterFunc(delay, retry)
}

// stop stops the engine gracefully, killing it after timeout. Callers hold s.op.
func (s *supervisor) stop(timeout time.Duration) {
	s.mu.Lock()
	s.want = false
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	p := s.proc
	s.mu.Unlock()
	if p == nil {
		return
	}
	syscall.Kill(p.pid, s.stopSignal)
	select {
	case <-p.done:
	case <-time.After(timeout):
		s.log.Warn("engine did not stop in time, killing", "pid", p.pid)
		syscall.Kill(-p.pid, syscall.SIGKILL)
		syscall.Kill(p.pid, syscall.SIGKILL)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
	}
}

// stable waits d and reports whether p is still running.
func (s *supervisor) stable(p *process, d time.Duration) bool {
	select {
	case <-p.done:
		return false
	case <-time.After(d):
		return true
	}
}
