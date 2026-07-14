//go:build darwin

package terminal

import "golang.org/x/sys/unix"

func enableTerminalSignals(fd int) error {
	termios, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return err
	}
	termios.Lflag |= unix.ISIG
	return unix.IoctlSetTermios(fd, unix.TIOCSETA, termios)
}
