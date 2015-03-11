package timeout

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

type Timeout struct {
	PreserveStatus bool
	Duration       uint64
	KillAfter      uint64
	Signal         os.Signal
	Cmd            *exec.Cmd
}

var defaultSignal = func() os.Signal {
	if runtime.GOOS == "windows" {
		return os.Interrupt
	}
	return syscall.SIGTERM
}()

// exit statuses are same with GNU timeout
const (
	exitNormal            = 0
	exitTimedOut          = 124
	exitUnknownErr        = 125
	exitCommandNotInvoked = 126
	exitCommandNotFound   = 127
	exitKilled            = 137
)

type tmError struct {
	ExitCode int
	message  string
}

func (err *tmError) Error() string {
	return err.message
}

type ExitStatus struct {
	Code int
	Type exitType
}

func (ex ExitStatus) String() string {
	return fmt.Sprintf("exitCode: %d, type: %s", ex.Code, ex.Type)
}

func (ex ExitStatus) isTimedOut() bool {
	return ex.Type == ExitTypeTimedOut
}

func (ex ExitStatus) isKilled() bool {
	return ex.Type == ExitTypeKilled
}

type exitType int

const (
	ExitTypeNormal exitType = iota + 1
	ExitTypeTimedOut
	ExitTypeKilled
)

func (eTyp exitType) String() string {
	switch eTyp {
	case ExitTypeNormal:
		return "normal"
	case ExitTypeTimedOut:
		return "timeout"
	case ExitTypeKilled:
		return "killed"
	default:
		return "unknown"
	}
}

func (tio *Timeout) signal() os.Signal {
	if tio.Signal == nil {
		return defaultSignal
	}
	return tio.Signal
}

func (tio *Timeout) Run() int {
	ch, stdoutPipe, stderrPipe, err := tio.RunCommand()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return int(err.ExitCode)
	}
	defer func() {
		stdoutPipe.Close()
		stderrPipe.Close()
	}()

	go readAndOut(stdoutPipe, os.Stdout)
	go readAndOut(stderrPipe, os.Stderr)

	exitSt := <-ch
	return int(exitSt.Code)
}

func (tio *Timeout) RunCommand() (exitChan chan ExitStatus, stdoutPipe, stderrPipe io.ReadCloser, tmerr *tmError) {
	cmd := tio.Cmd

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		tmerr = &tmError{
			ExitCode: exitUnknownErr,
			message:  fmt.Sprintf("unknown error: %s", err),
		}
		return
	}
	stderrPipe, err = cmd.StderrPipe()
	if err != nil {
		tmerr = &tmError{
			ExitCode: exitUnknownErr,
			message:  fmt.Sprintf("unknown error: %s", err),
		}
		return
	}
	if err = cmd.Start(); err != nil {
		switch {
		case os.IsNotExist(err):
			tmerr = &tmError{
				ExitCode: exitCommandNotFound,
				message:  err.Error(),
			}
		case os.IsPermission(err):
			tmerr = &tmError{
				ExitCode: exitCommandNotInvoked,
				message:  err.Error(),
			}
		default:
			tmerr = &tmError{
				ExitCode: exitUnknownErr,
				message:  fmt.Sprintf("unknown error: %s", err),
			}
		}
		return
	}

	exitChan = make(chan ExitStatus)
	go func() {
		exitChan <- tio.handleTimeout()
	}()

	return
}

func (tio *Timeout) handleTimeout() (ex ExitStatus) {
	cmd := tio.Cmd
	exitChan := getExitChan(cmd)
	if tio.Duration > 0 {
		select {
		case exitCode := <-exitChan:
			ex.Code = exitCode
			ex.Type = ExitTypeNormal
		case <-time.After(time.Duration(tio.Duration) * time.Second):
			cmd.Process.Signal(tio.signal())
			ex.Code = exitTimedOut
			ex.Type = ExitTypeTimedOut
		}
	} else {
		exitCode := <-exitChan
		return ExitStatus{
			Code: exitCode,
			Type: ExitTypeNormal,
		}
	}

	if ex.isTimedOut() {
		tmpExit := exitNormal
		if tio.KillAfter > 0 {
			select {
			case tmpExit = <-exitChan:
			case <-time.After(time.Duration(tio.KillAfter) * time.Second):
				cmd.Process.Kill()
				ex.Code = exitKilled
				ex.Type = ExitTypeKilled
			}
		} else {
			tmpExit = <-exitChan
		}
		if tio.PreserveStatus && !ex.isKilled() {
			ex.Code = tmpExit
		}
	}

	return ex
}

func getExitChan(cmd *exec.Cmd) chan int {
	ch := make(chan int)
	go func() {
		err := cmd.Wait()
		ch <- resolveCode(err)
	}()
	return ch
}

func resolveCode(err error) int {
	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				return status.ExitStatus()
			}
		}
		// XXX The exit codes in some platforms aren't integer. e.g. plan9.
		return -1
	}
	return exitNormal
}

func readAndOut(r io.Reader, f *os.File) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		fmt.Fprintln(f, s.Text())
	}
}

var durRe = regexp.MustCompile(`^([0-9]+)([smhd])?$`)

func parseDuration(durStr string) (uint64, error) {
	matches := durRe.FindStringSubmatch(durStr)
	if len(matches) == 0 {
		return 0, fmt.Errorf("duration format invalid: %s", durStr)
	}

	base, _ := strconv.ParseUint(matches[1], 10, 64)
	switch matches[2] {
	case "", "s":
		return base, nil
	case "m":
		return base * 60, nil
	case "h":
		return base * 60 * 60, nil
	case "d":
		return base * 60 * 60 * 24, nil
	default:
		return 0, fmt.Errorf("something went wrong")
	}
}
