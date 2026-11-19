package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The fixture reads inputs only when it boots. These checks prove that the CLI
// whitelist controls actual backend refreshes, independently of gopls behavior.
func TestRawProxyIncludeWhitelist(t *testing.T) {
	for _, mode := range []string{"watcher", "polling", "checkpoints"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			selected := filepath.Join(f.workspace, "src/selected.go")
			added := filepath.Join(f.workspace, "src/new/deep/added.go")
			write := func(path, value string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(selected, "A")
			f.inputs = append(f.inputs, selected, added)
			flags := []string{"--include", "src/**/*.go", "--include", "go.mod", "--exclude", "src/generated/**"}
			switch mode {
			case "watcher":
				flags = append(flags, "--disable-watcher=false")
			case "polling":
				flags = append(flags, "--poll-interval", "20ms")
			}
			p := f.proxyFlags(t, flags)
			p.initialize("2025-11-25")
			previous := p.snapshot("1", false)
			sequence := 1
			snapshot := func() {
				t.Helper()
				sequence++
				previous = p.snapshot(fmt.Sprint(sequence), false)
			}
			unchanged := func() {
				t.Helper()
				boot := previous.Boot
				// Allow asynchronous events and multiple idle polls to settle,
				// also exercising request checks throughout the observation window.
				for until := time.Now().Add(150 * time.Millisecond); time.Now().Before(until); {
					snapshot()
					if previous.Boot != boot {
						t.Fatal("unmatched input restarted the backend")
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			changed := func() {
				t.Helper()
				boot := previous.Boot
				if mode != "checkpoints" {
					// No request is sent until a new boot is recorded: watcher and
					// poll modes must notice additions/deletions while idle.
					deadline := time.Now().Add(5 * time.Second)
					for {
						seen := false
						for _, event := range f.events(t) {
							if event.Kind == "boot" {
								seen = event.Boot != boot
							}
						}
						if seen {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("matching change did not refresh idle backend")
						}
						time.Sleep(5 * time.Millisecond)
					}
				}
				snapshot()
				if previous.Boot == boot {
					t.Fatal("matching input reused the old backend")
				}
			}

			write(filepath.Join(f.workspace, "go.sum"), "unmatched")
			write(filepath.Join(f.workspace, "docs/nested/ignored.go"), "unmatched")
			write(filepath.Join(f.workspace, "src/notes/readme.txt"), "unmatched")
			write(filepath.Join(f.workspace, "src/generated/ignored.go"), "excluded")
			if err := os.Mkdir(filepath.Join(f.workspace, "src/empty.go"), 0700); err != nil {
				t.Fatal(err)
			}
			unchanged()
			if err := os.Rename(filepath.Join(f.workspace, "src/notes"), filepath.Join(f.workspace, "src/moved")); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(f.workspace, "src/empty.go")); err != nil {
				t.Fatal(err)
			}
			unchanged()

			write(added, "new")
			changed()
			requireInput(t, previous, "added.go", "new")
			writeInput(t, selected, "B", true) // Same size and original mtime.
			changed()
			requireInput(t, previous, "selected.go", "B")
			temporary := filepath.Join(f.workspace, "src/save.tmp")
			write(temporary, "C")
			if err := os.Rename(temporary, selected); err != nil {
				t.Fatal(err)
			}
			changed()
			requireInput(t, previous, "selected.go", "C")
			if err := os.Rename(selected, selected+".txt"); err != nil {
				t.Fatal(err)
			}
			changed()
			if previous.Inputs["selected.go"] != nil {
				t.Fatal("renamed-out input remains in backend snapshot")
			}
			if err := os.Rename(selected+".txt", selected); err != nil {
				t.Fatal(err)
			}
			changed()
			requireInput(t, previous, "selected.go", "C")
			if err := os.RemoveAll(filepath.Join(f.workspace, "src/new")); err != nil {
				t.Fatal(err)
			}
			changed()
			if previous.Inputs["added.go"] != nil {
				t.Fatal("deleted directory left stale input in backend")
			}
			staged := filepath.Join(f.root, "staged")
			write(filepath.Join(staged, "deep/added.go"), "moved")
			if err := os.Rename(staged, filepath.Join(f.workspace, "src/new")); err != nil {
				t.Fatal(err)
			}
			changed()
			requireInput(t, previous, "added.go", "moved")
			if err := os.Rename(filepath.Join(f.workspace, "src/new"), staged); err != nil {
				t.Fatal(err)
			}
			changed()
			if previous.Inputs["added.go"] != nil {
				t.Fatal("moved-out directory left stale input in backend")
			}
			write(filepath.Join(f.workspace, "go.mod"), "second pattern")
			changed()
			requireInput(t, previous, "go.mod", "second pattern")
		})
	}
}
