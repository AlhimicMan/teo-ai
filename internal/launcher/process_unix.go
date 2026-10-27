//go:build darwin || linux

package launcher

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

type childExit struct{ code int }

func (e *childExit) Error() string { return "native client exited" }

// Inherit the terminal and working directory. Supervise instead of exec so that
// Claude's private settings file can be removed after the native process exits.
func runChild(cmd *exec.Cmd) error {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return errors.New("cannot start native client")
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-signals:
				_ = cmd.Process.Signal(sig)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			code = 128 + int(status.Signal())
		}
		return &childExit{code: code}
	}
	return err
}
