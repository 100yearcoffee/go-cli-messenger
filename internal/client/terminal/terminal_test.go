package terminal

import (
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	t.Parallel()
	input := "hello\x1b[31m red\nnext\r\u202Etxt\x00 世界"
	want := `hello\u{001B}[31m red\nnext\r\u{202E}txt\u{0000} 世界`
	if got := Sanitize(input); got != want {
		t.Fatalf("Sanitize() = %q, want %q", got, want)
	}
}

func TestSanitizeTerminalAttackClasses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"CSI color", "\x1b[31m", `\u{001B}[31m`},
		{"OSC title", "\x1b]0;owned\x07", `\u{001B}]0;owned\u{0007}`},
		{"C0", "a\x00b", `a\u{0000}b`},
		{"C1", "a\u0085b", `a\u{0085}b`},
		{"bidi override", "a\u202Eb", `a\u{202E}b`},
		{"invalid UTF-8", string([]byte{'a', 0x80, 0xff, 'b'}), `a\x80\xFFb`},
		{"newlines visible", "a\nb\rc", `a\nb\rc`},
		{"tab preserved", "a\tb", "a\tb"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := Sanitize(test.input); got != test.want {
				t.Fatalf("Sanitize(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestSanitizeLongUnicodeText(t *testing.T) {
	t.Parallel()
	input := strings.Repeat("世界🙂", 1000)
	if got := Sanitize(input); got != input {
		t.Fatal("safe Unicode text was changed")
	}
}

func TestRightAlign(t *testing.T) {
	t.Parallel()
	if got, want := RightAlign("hello <you", 16), "      hello <you"; got != want {
		t.Fatalf("RightAlign() = %q, want %q", got, want)
	}
	if got := RightAlign("peer> message", 5); got != "peer> message" {
		t.Fatalf("narrow RightAlign() = %q", got)
	}
}

func TestScreenLifecycleResetsTerminalFeatures(t *testing.T) {
	t.Parallel()
	if !strings.Contains(enterScreen, "?1049h") {
		t.Fatal("interactive lifecycle does not enter the alternate screen")
	}
	for _, reset := range []string{"[0m", "?25h", "?1000l", "?1006l", "?1049l"} {
		if !strings.Contains(leaveScreen, reset) {
			t.Fatalf("terminal restore is missing %q", reset)
		}
	}
}

func TestVideoRendererUsesCompactFullRedrawForNoisyFrame(t *testing.T) {
	const columns, rows = 100, 40
	previous := []byte(strings.Repeat("a", columns*rows))
	cells := []byte(strings.Repeat("a", columns*rows))
	for index := range cells {
		if index%2 == 0 {
			cells[index] = 'b'
		}
	}
	output := renderVideoUpdate(previous, columns, rows, columns, rows, cells)
	if len(output) > len(cells)+rows*12 {
		t.Fatalf("noisy frame output has %d bytes for %d cells", len(output), len(cells))
	}
	if strings.Count(output, "H") != rows {
		t.Fatalf("full redraw used %d cursor moves, want %d", strings.Count(output, "H"), rows)
	}
}

func TestVideoRendererKeepsSmallIncrementalUpdate(t *testing.T) {
	previous := []byte("abcdefgh")
	cells := []byte("abcXefgh")
	output := renderVideoUpdate(previous, 4, 2, 4, 2, cells)
	if strings.Contains(output, "abcd") || !strings.Contains(output, "X") {
		t.Fatalf("small update was not rendered incrementally: %q", output)
	}
}
