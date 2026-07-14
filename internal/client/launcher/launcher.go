package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var ErrNoTerminal = errors.New("no supported terminal emulator found")

type Command struct {
	Program string
	Args    []string
}

// Resolve builds a terminal command using separate arguments throughout. The
// call ID is never interpreted by a shell.
func Resolve(configured, executable, callID string, lookup func(string) (string, error)) (Command, error) {
	if executable == "" || callID == "" {
		return Command{}, errors.New("termcall executable and call ID are required")
	}
	if lookup == nil {
		lookup = exec.LookPath
	}
	candidates := []string{
		configured,
		os.Getenv("TERMINAL"),
		"kitty",
		"wezterm",
		"alacritty",
		"foot",
		"gnome-terminal",
		"konsole",
		"xterm",
	}
	for _, candidate := range candidates {
		program := strings.TrimSpace(candidate)
		// Environment/configuration values containing arguments are rejected;
		// accepting them would require shell parsing and create an injection path.
		if program == "" || strings.ContainsAny(program, "\r\n\t ") {
			continue
		}
		path, err := lookup(program)
		if err != nil {
			continue
		}
		arguments := terminalPrefix(program)
		arguments = append(arguments, executable, "answer", callID)
		return Command{Program: path, Args: arguments}, nil
	}
	if runtime.GOOS == "darwin" {
		path, err := lookup("osascript")
		if err == nil {
			return macOSTerminalCommand(path, executable, callID), nil
		}
	}
	return Command{}, ErrNoTerminal
}

func macOSTerminalCommand(osascript, executable, callID string) Command {
	const script = `on run argv
set executablePath to item 1 of argv
set callID to item 2 of argv
tell application "Terminal"
activate
do script (quoted form of executablePath) & " answer " & (quoted form of callID)
end tell
end run`
	return Command{Program: osascript, Args: []string{"-e", script, "--", executable, callID}}
}

func terminalPrefix(program string) []string {
	switch filepath.Base(program) {
	case "wezterm":
		return []string{"start", "--"}
	case "alacritty", "konsole", "xterm":
		return []string{"-e"}
	case "foot", "gnome-terminal":
		return []string{"--"}
	default:
		return nil
	}
}

func Launch(configured, callID string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate termcall executable: %w", err)
	}
	command, err := Resolve(configured, executable, callID, exec.LookPath)
	if err != nil {
		return err
	}
	process := exec.Command(command.Program, command.Args...)
	process.Stdin = nil
	process.Stdout = nil
	process.Stderr = nil
	if err := process.Start(); err != nil {
		return fmt.Errorf("launch terminal: %w", err)
	}
	go func() { _ = process.Wait() }()
	return nil
}
