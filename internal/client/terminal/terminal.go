package terminal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	xterm "golang.org/x/term"
)

type Line struct {
	Text string
	Err  error
}

type UI struct {
	input  io.Reader
	output io.Writer
	errors io.Writer
	mu     sync.Mutex

	interactive bool
	editor      *xterm.Terminal
	inputFD     int
	outputFD    int
	oldState    *xterm.State
	restoreOnce sync.Once
}

func New(input io.Reader, output, errorsOutput io.Writer) *UI {
	u := &UI{input: input, output: output, errors: errorsOutput}
	inputFile, inputOK := input.(*os.File)
	outputFile, outputOK := output.(*os.File)
	if !inputOK || !outputOK {
		return u
	}
	inputFD, outputFD := int(inputFile.Fd()), int(outputFile.Fd())
	if !xterm.IsTerminal(inputFD) || !xterm.IsTerminal(outputFD) {
		return u
	}
	oldState, err := xterm.MakeRaw(inputFD)
	if err != nil {
		return u
	}
	// Keep terminal-generated signals such as Ctrl+C while retaining raw-mode
	// line editing for every other key.
	termios, err := unix.IoctlGetTermios(inputFD, unix.TCGETS)
	if err != nil {
		_ = xterm.Restore(inputFD, oldState)
		return u
	}
	termios.Lflag |= unix.ISIG
	if err := unix.IoctlSetTermios(inputFD, unix.TCSETS, termios); err != nil {
		_ = xterm.Restore(inputFD, oldState)
		return u
	}
	editor := xterm.NewTerminal(terminalReadWriter{Reader: input, Writer: output}, "> ")
	if width, height, sizeErr := xterm.GetSize(outputFD); sizeErr == nil {
		_ = editor.SetSize(width, height)
	}
	u.interactive = true
	u.editor = editor
	u.inputFD = inputFD
	u.outputFD = outputFD
	u.oldState = oldState
	return u
}

type terminalReadWriter struct {
	io.Reader
	io.Writer
}

func (u *UI) Lines(ctx context.Context) <-chan Line {
	if u.interactive {
		return u.terminalLines(ctx)
	}
	lines := make(chan Line)
	scanner := bufio.NewScanner(u.input)
	scanner.Buffer(make([]byte, 4096), 8<<10)
	go func() {
		defer close(lines)
		for scanner.Scan() {
			select {
			case lines <- Line{Text: scanner.Text()}:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case lines <- Line{Err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return lines
}

func (u *UI) terminalLines(ctx context.Context) <-chan Line {
	lines := make(chan Line)
	go func() {
		defer close(lines)
		for {
			text, err := u.editor.ReadLine()
			if errors.Is(err, xterm.ErrPasteIndicator) {
				err = nil
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					select {
					case lines <- Line{Err: err}:
					case <-ctx.Done():
					}
				}
				return
			}
			// ReadLine has already echoed the committed line. Remove that copy;
			// Local will render the accepted message once, aligned to the right.
			_, _ = u.editor.Write([]byte("\x1b[1A\r\x1b[2K"))
			select {
			case lines <- Line{Text: text}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return lines
}

func (u *UI) System(format string, arguments ...any) {
	u.write(u.output, "[system] "+format+"\n", arguments...)
}

func (u *UI) Prompt(format string, arguments ...any) {
	u.write(u.output, format, arguments...)
}

func (u *UI) Local(text string) {
	message := Sanitize(text) + " <you"
	if u.interactive {
		if width, _, err := xterm.GetSize(u.outputFD); err == nil {
			message = RightAlign(message, width)
		}
	}
	u.write(u.output, "%s\n", message)
}

func (u *UI) Remote(username, text string) {
	u.write(u.output, "%s> %s\n", Sanitize(username), Sanitize(text))
}

func (u *UI) Error(format string, arguments ...any) {
	u.write(u.errors, "[error] "+format+"\n", arguments...)
}

// Restore returns an interactive terminal to its original mode.
func (u *UI) Restore() {
	u.restoreOnce.Do(func() {
		if u.oldState != nil {
			_ = xterm.Restore(u.inputFD, u.oldState)
			// ReadLine may already have drawn the next prompt before the app
			// decided to exit. Remove it after restoring cooked terminal mode.
			_, _ = io.WriteString(u.output, "\r\x1b[2K")
		}
	})
}

func (u *UI) write(destination io.Writer, format string, arguments ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	message := fmt.Sprintf(format, arguments...)
	if u.interactive {
		_, _ = u.editor.Write([]byte(message))
		return
	}
	_, _ = io.WriteString(destination, message)
}

func RightAlign(value string, width int) string {
	visibleWidth := utf8.RuneCountInString(value)
	if width <= visibleWidth {
		return value
	}
	return strings.Repeat(" ", width-visibleWidth) + value
}

// Sanitize renders untrusted peer text without allowing terminal control,
// escape, bidi, or invalid UTF-8 sequences to affect the local terminal.
func Sanitize(value string) string {
	result := make([]byte, 0, len(value))
	for len(value) > 0 {
		firstByte := value[0]
		r, size := utf8.DecodeRuneInString(value)
		value = value[size:]
		switch {
		case r == utf8.RuneError && size == 1:
			result = fmt.Appendf(result, "\\x%02X", firstByte)
		case r == '\t':
			result = append(result, '\t')
		case r == '\n':
			result = append(result, "\\n"...)
		case r == '\r':
			result = append(result, "\\r"...)
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf):
			result = fmt.Appendf(result, "\\u{%04X}", r)
		default:
			result = utf8.AppendRune(result, r)
		}
	}
	return string(result)
}
