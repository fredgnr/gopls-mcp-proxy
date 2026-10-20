// Package launcher selects the default gopls command and rejects direct or
// file-alias references to the proxy. It has no protocol or workspace knowledge.
package launcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// fileSystem keeps filesystem failures independently testable, including under
// privileged test runners where chmod cannot reliably produce EACCES.
type fileSystem struct {
	lstat        func(string) (os.FileInfo, error)
	stat         func(string) (os.FileInfo, error)
	evalSymlinks func(string) (string, error)
}

func localFS() fileSystem {
	return fileSystem{lstat: os.Lstat, stat: os.Stat, evalSymlinks: filepath.EvalSymlinks}
}

// SelfIdentity retains both the startup file identity and its physical locator.
// Each check also reads the current locator, so replacing the proxy cannot hide
// aliases to either its old running file or its new installed file.
type SelfIdentity struct {
	path    string
	initial os.FileInfo
	fs      fileSystem
}

// CaptureSelf captures the actual executable, never argv[0] or the project cwd.
func CaptureSelf() (*SelfIdentity, error) {
	path, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("determine proxy executable: %w", err)
	}
	return CaptureIdentity(path)
}

// CaptureIdentity captures a specified executable identity. Production callers
// use CaptureSelf; explicit identity injection supports component/host tests.
func CaptureIdentity(path string) (*SelfIdentity, error) {
	return captureIdentity(path, localFS())
}

func captureIdentity(path string, fs fileSystem) (*SelfIdentity, error) {
	if path == "" {
		return nil, fmt.Errorf("determine proxy identity: executable path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("determine proxy absolute path: %w", err)
	}
	physical, err := fs.evalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("determine proxy physical path %q: %w", abs, err)
	}
	info, err := fs.stat(physical)
	if err != nil {
		return nil, fmt.Errorf("determine proxy identity %q: %w", physical, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return nil, fmt.Errorf("proxy identity %q is not a regular executable", physical)
	}
	return &SelfIdentity{path: filepath.Clean(physical), initial: info, fs: fs}, nil
}

// Path returns the physical absolute path captured at startup.
func (s *SelfIdentity) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Check validates a candidate immediately before spawn without changing its
// locator. Path inspection and exec are separate operations: this catches
// observed misconfiguration, not every concurrent replacement or script loop.
func (s *SelfIdentity) Check(ctx context.Context, locator string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.initial == nil || s.path == "" {
		return fmt.Errorf("proxy identity is unavailable")
	}
	if locator == "" {
		return fmt.Errorf("backend locator is empty")
	}
	abs, err := filepath.Abs(locator)
	if err != nil {
		return fmt.Errorf("resolve backend locator %q: %w", locator, err)
	}
	if abs == s.path {
		return fmt.Errorf("backend %q refers to the proxy itself", locator)
	}
	// Stat follows candidate aliases, while preserving the configured locator for
	// future upgrades. No file is opened here, including FIFOs and devices.
	candidate, err := s.fs.stat(abs)
	if err != nil {
		return fmt.Errorf("inspect backend candidate %q (missing, inaccessible or broken link): %w", locator, err)
	}
	if !candidate.Mode().IsRegular() || candidate.Mode()&0111 == 0 {
		return fmt.Errorf("backend candidate %q is not a regular executable", locator)
	}
	current, err := s.fs.stat(s.path)
	if err != nil {
		return fmt.Errorf("inspect current proxy identity %q: %w", s.path, err)
	}
	if !current.Mode().IsRegular() || current.Mode()&0111 == 0 {
		return fmt.Errorf("current proxy identity %q is not a regular executable", s.path)
	}
	if os.SameFile(candidate, s.initial) || os.SameFile(candidate, current) {
		return fmt.Errorf("backend %q refers to the proxy itself", locator)
	}
	return ctx.Err()
}
