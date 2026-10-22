//go:build !darwin

package integration

import "time"

func refineLifecycleIdentity(p lifecycleProcess, _ time.Time) (lifecycleProcess, error) {
	return p, nil
}
