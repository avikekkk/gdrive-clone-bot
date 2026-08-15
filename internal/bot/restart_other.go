//go:build !unix

package bot

import "errors"

// restartProcess is unsupported outside Unix, where there is no exec that
// replaces the current process.
func restartProcess() error {
	return errors.New("restart is not supported on this platform")
}
