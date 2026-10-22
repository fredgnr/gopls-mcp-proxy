//go:build darwin

package integration

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// KERN_PROC_PID returns an empty result when a process disappears. The slice
// API preserves that distinction; SysctlKinfoProc turns an empty result into EIO.
func lifecycleDarwinProc(pid int) (*unix.KinfoProc, error) {
	rows, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, unix.ESRCH
	}
	if len(rows) != 1 || int(rows[0].Proc.P_pid) != pid {
		return nil, fmt.Errorf("ambiguous kernel process identity for pid %d", pid)
	}
	return &rows[0], nil
}

// Use kernel birth/parent/state from one snapshot. A process born after ps
// started cannot safely inherit that row's command/RSS, so retry next sample.
func refineLifecycleIdentity(p lifecycleProcess, snapshotStarted time.Time) (lifecycleProcess, error) {
	info, err := lifecycleDarwinProc(p.PID)
	if err != nil {
		return p, err
	}
	start := info.Proc.P_starttime
	if time.Unix(start.Sec, int64(start.Usec)*1000).After(snapshotStarted) {
		return p, unix.ESRCH
	}
	expected := strings.Join(strings.Fields(time.Unix(start.Sec, 0).Local().Format("Mon Jan _2 15:04:05 2006")), " ")
	if expected != p.Start {
		return p, unix.ESRCH
	}
	p.Start = fmt.Sprintf("%d.%06d", start.Sec, start.Usec)
	p.PPID = int(info.Eproc.Ppid)
	p.PGID = int(info.Eproc.Pgid)
	if info.Proc.P_stat == 5 {
		// getsid does not find zombies on Darwin. A confirmed zombie still exists:
		// it must not be silently converted into an absent/reaped process.
		p.State = "Z"
		p.SID = "unavailable:zombie"
		return p, nil
	}
	p.State = "S"
	sid, sidErr := unix.Getsid(p.PID)
	again, err := lifecycleDarwinProc(p.PID)
	if err != nil {
		return p, err
	}
	if again.Proc.P_starttime != start {
		return p, unix.ESRCH
	}
	p.PPID = int(again.Eproc.Ppid)
	p.PGID = int(again.Eproc.Pgid)
	if again.Proc.P_stat == 5 {
		p.State = "Z"
		p.SID = "unavailable:zombie"
		return p, nil
	}
	if sidErr != nil {
		return p, fmt.Errorf("session unavailable for still-present pid %d: %v", p.PID, sidErr)
	}
	p.SID = strconv.Itoa(sid)
	return p, nil
}
