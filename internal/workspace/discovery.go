package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/mod/modfile"
)

type discoveryObservation struct {
	exists     bool
	content    *Fingerprint
	linkTarget *string
}

func (t *Tracker) observeDiscoveryPath(p string) (bool, error) {
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		t.discoveryObservations[p] = discoveryObservation{exists: false}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	observed := discoveryObservation{exists: true}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(p)
		if err != nil {
			return false, err
		}
		observed.linkTarget = &target
	}
	t.discoveryObservations[p] = observed
	return true, nil
}

// Discovery is optional and does not validate Go source or module syntax.
// go.work parsing only supplies additional roots; syntax/size failures retain
// the last successful scope. File access failures remain consistency errors.
func (t *Tracker) discover(ctx context.Context) ([]string, []string, bool, error) {
	t.discoveryObservations = map[string]discoveryObservation{}
	if !t.cfg.GoDiscovery {
		return nil, nil, false, nil
	}
	roots, dependencies := map[string]bool{}, map[string]bool{}
	exists := func(p string) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		dependencies[p] = true
		return t.observeDiscoveryPath(p)
	}
	work, foundMod := t.cfg.WorkFile, false
	for dir := t.cfg.Workspace; ; dir = filepath.Dir(dir) {
		if !foundMod {
			present, err := exists(filepath.Join(dir, "go.mod"))
			if err != nil {
				return nil, nil, false, err
			}
			if present {
				roots[dir], foundMod = true, true
				dependencies[filepath.Join(dir, "go.sum")] = true
			}
		}
		if work == "" {
			candidate := filepath.Join(dir, "go.work")
			present, err := exists(candidate)
			if err != nil {
				return nil, nil, false, err
			}
			if present {
				work = candidate
			}
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	var discoveryErr error
	if work != "" && work != "off" {
		dependencies[work], dependencies[work+".sum"] = true, true
		if _, err := t.observeDiscoveryPath(work); err != nil {
			return nil, nil, false, err
		}
		resolved, err := filepath.EvalSymlinks(work)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, false, err
		}
		if resolved == "" {
			resolved = work
		}
		dependencies[resolved] = true
		if _, err := t.observeDiscoveryPath(resolved); err != nil {
			return nil, nil, false, err
		}
		file, err := os.Open(resolved)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				discoveryErr = fmt.Errorf("go.work unavailable: %s", work)
			} else {
				return nil, nil, false, err
			}
		} else {
			// Parsing is a convenience, so excessively large Go metadata degrades
			// discovery without bypassing the later streaming content fingerprint.
			const maxDiscoveryBytes = 8 * 1024 * 1024
			bytes, readErr := io.ReadAll(io.LimitReader(contextReader{ctx, file}, maxDiscoveryBytes+1))
			closeErr := file.Close()
			if readErr != nil {
				return nil, nil, false, readErr
			}
			if closeErr != nil {
				return nil, nil, false, closeErr
			}
			if len(bytes) > maxDiscoveryBytes {
				discoveryErr = fmt.Errorf("go.work exceeds optional discovery parse limit")
			} else {
				digest := Fingerprint(sha256.Sum256(bytes))
				t.discoveryObservations[resolved] = discoveryObservation{exists: true, content: &digest}
				parsed, err := modfile.ParseWork(work, bytes, nil)
				if err != nil {
					discoveryErr = err
				} else {
					for _, use := range parsed.Use {
						root := use.Path
						if !filepath.IsAbs(root) {
							root = filepath.Join(filepath.Dir(work), root)
						}
						roots[filepath.Clean(root)] = true
					}
				}
			}
		}
	}
	if discoveryErr != nil {
		t.logger.Warn("Go scope discovery degraded; retaining last successful roots", "error", discoveryErr, "retained_roots", t.discovered)
		for _, dep := range t.discoveryDependencies {
			dependencies[dep] = true
		}
		return t.discovered, keys(dependencies), true, nil
	}
	return keys(roots), keys(dependencies), false, nil
}
