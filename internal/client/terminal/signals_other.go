//go:build !linux && !darwin

package terminal

import "errors"

func enableTerminalSignals(int) error {
	return errors.New("interactive terminal mode is unsupported on this platform")
}
