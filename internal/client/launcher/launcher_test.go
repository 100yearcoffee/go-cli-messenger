package launcher

import (
	"errors"
	"reflect"
	"testing"
)

func TestResolveKeepsCallIDAsOneArgument(t *testing.T) {
	lookup := func(program string) (string, error) {
		if program == "kitty" {
			return "/usr/bin/kitty", nil
		}
		return "", errors.New("not found")
	}
	command, err := Resolve("kitty", "/usr/bin/termcall", "call-id;touch /tmp/bad", lookup)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/termcall", "answer", "call-id;touch /tmp/bad"}
	if command.Program != "/usr/bin/kitty" || !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("got %#v, want program /usr/bin/kitty args %#v", command, want)
	}
}
