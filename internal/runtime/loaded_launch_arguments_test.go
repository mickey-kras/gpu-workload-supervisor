package runtime

import (
	"errors"
	"testing"
)

func TestLoadedLaunchCommandsPreserveArgumentBoundaries(t *testing.T) {
	command := `/usr/bin/worker "a b" c`
	matching := `{ path=/usr/bin/worker ; argv[]=/usr/bin/worker 'a b' c ; ignore_errors=no ; }`
	changed := `{ path=/usr/bin/worker ; argv[]=/usr/bin/worker a 'b c' ; ignore_errors=no ; }`
	for _, check := range []struct {
		name string
		run  func(string) error
	}{
		{"main", func(output string) error { return CheckLoadedLaunchCommand(output, command) }},
		{"preparation", func(output string) error { return CheckLoadedPreCommands(output, []string{command}) }},
	} {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(matching); err != nil {
				t.Fatal("equivalent quoting rejected", err)
			}
			if err := check.run(changed); !errors.Is(err, ErrLaunchChanged) {
				t.Fatal("changed argument boundaries accepted", err)
			}
		})
	}
}
