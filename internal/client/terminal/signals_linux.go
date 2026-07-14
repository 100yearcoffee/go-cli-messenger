//go:build linux

package terminal

import "golang.org/x/sys/unix"

func enableTerminalSignals(fd int) error {
	termios, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	termios.Lflag |= unix.ISIG
	return unix.IoctlSetTermios(fd, unix.TCSETS, termios)
}
