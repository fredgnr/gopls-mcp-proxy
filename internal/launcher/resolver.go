package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Spec is a default launch decision. Locator remains fixed for the lifetime of
// the proxy, but a symlink at that locator may point to an upgraded backend.
type Spec struct {
	Locator         string
	Args            []string
	SelectionSource string
}

// ResolveDefault tries the physical proxy sibling, then absolute PATH entries.
// Only a truly absent candidate permits fallback; the first existing candidate
// is binding even when invalid. This intentionally differs from explicit command
// resolution, whose historical PATH semantics live in package backend.
func ResolveDefault(self *SelfIdentity, pathEnv string) (Spec, error) {
	if self == nil || self.initial == nil || self.path == "" {
		return Spec{}, fmt.Errorf("default gopls discovery requires proxy identity")
	}
	sibling := filepath.Join(filepath.Dir(self.path), "gopls")
	try := func(path, source string) (Spec, bool, error) {
		_, err := self.fs.lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return Spec{}, false, nil
		}
		if err != nil {
			return Spec{}, true, fmt.Errorf("inspect %s gopls candidate %q: %w", source, path, err)
		}
		if err := self.Check(context.Background(), path); err != nil {
			return Spec{}, true, fmt.Errorf("invalid %s gopls candidate: %w", source, err)
		}
		return Spec{Locator: filepath.Clean(path), Args: []string{"mcp"}, SelectionSource: source}, true, nil
	}
	if spec, found, err := try(sibling, "sibling"); found {
		return spec, err
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if !filepath.IsAbs(dir) {
			continue
		}
		if spec, found, err := try(filepath.Join(dir, "gopls"), "PATH"); found {
			return spec, err
		}
	}
	return Spec{}, fmt.Errorf("default gopls not found: no candidate at sibling %q or in absolute PATH directories; install gopls or use -- program [args...]", sibling)
}
