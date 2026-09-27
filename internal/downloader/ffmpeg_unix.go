//go:build unix

package downloader

import "syscall"

// ownProcessGroup starts a process in a new process group, so signals sent to
// the terminal's foreground group don't reach it.
func ownProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
