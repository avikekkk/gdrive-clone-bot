//go:build unix

package bot

import (
	"os"
	"syscall"
)

// restartProcess replaces the running bot with a fresh copy of the same
// binary, keeping the pid so a supervisor (or bot.sh) does not see an exit.
// On success it never returns.
func restartProcess() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(executable, os.Args, os.Environ())
}
