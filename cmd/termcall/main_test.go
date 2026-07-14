package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestDaemonChildArgumentsExcludeDetachedFlag(t *testing.T) {
	got := daemonChildArguments("/tmp/termcall.sock", "print", true, "kitty")
	want := []string{
		"daemon", "--socket", "/tmp/termcall.sock", "--incoming", "print",
		"--do-not-disturb", "--terminal", "kitty",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for _, argument := range got {
		if strings.Contains(argument, "detached") {
			t.Fatalf("child argument %q would recursively detach", argument)
		}
	}
}

func TestDaemonStopCommandIncludesPID(t *testing.T) {
	if command := daemonStopCommand(1234); !strings.Contains(command, "1234") {
		t.Fatalf("stop command %q does not contain PID", command)
	}
}
