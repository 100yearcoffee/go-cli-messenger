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

const (
	enterScreen = "\x1b[?1049h\x1b[2J\x1b[H"
	leaveScreen = "\x1b[0m\x1b[?25h\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1049l"
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
	videoCells  []byte
	videoWidth  int
	videoHeight int
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
	// The alternate buffer keeps video redraws and status output out of the
	// user's shell history. Restore emits a complete defensive reset on every
	// normal, error, and signal-driven exit path.
	_, _ = io.WriteString(output, enterScreen)
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

// VideoSize clamps a configured profile to the current terminal viewport,
// leaving four rows for status and chat input.
func (u *UI) VideoSize(maxColumns, maxRows int) (int, int) {
	if !u.interactive {
		return maxColumns, maxRows
	}
	width, height, err := xterm.GetSize(u.outputFD)
	if err != nil {
		return maxColumns, maxRows
	}
	if width < maxColumns {
		maxColumns = width
	}
	if availableRows := height - 4; availableRows < maxRows {
		maxRows = availableRows
	}
	if maxColumns < 1 {
		maxColumns = 1
	}
	if maxRows < 1 {
		maxRows = 1
	}
	return maxColumns, maxRows
}

// RenderVideo updates only changed printable ASCII cells in a fixed viewport
// at the top-left of an interactive terminal. Cursor save/restore keeps the
// active chat input intact while frames arrive.
func (u *UI) RenderVideo(columns, rows int, cells []byte) {
	if !u.interactive || columns < 1 || rows < 1 || len(cells) != columns*rows {
		return
	}
	for _, cell := range cells {
		if cell < 0x20 || cell > 0x7e {
			return
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	full := u.videoWidth != columns || u.videoHeight != rows || len(u.videoCells) != len(cells)
	var output strings.Builder
	output.Grow(len(cells) + rows*12)
	output.WriteString("\x1b[s")
	for row := 0; row < rows; row++ {
		rowStart := row * columns
		for column := 0; column < columns; {
			index := rowStart + column
			if !full && u.videoCells[index] == cells[index] {
				column++
				continue
			}
			start := column
			for column < columns {
				index = rowStart + column
				if !full && u.videoCells[index] == cells[index] {
					break
				}
				column++
			}
			fmt.Fprintf(&output, "\x1b[%d;%dH", row+1, start+1)
			output.Write(cells[rowStart+start : rowStart+column])
		}
	}
	output.WriteString("\x1b[u")
	_, _ = io.WriteString(u.output, output.String())
	u.videoCells = append(u.videoCells[:0], cells...)
	u.videoWidth, u.videoHeight = columns, rows
}

// Restore returns an interactive terminal to its original mode.
func (u *UI) Restore() {
	u.restoreOnce.Do(func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		if u.oldState != nil {
			_ = xterm.Restore(u.inputFD, u.oldState)
			// Also undo cursor visibility, SGR, mouse modes, and the alternate
			// screen even when a media or network error caused the exit.
			_, _ = io.WriteString(u.output, leaveScreen)
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
