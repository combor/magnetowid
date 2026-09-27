package downloader

import "syscall"

// ownProcessGroup starts a process in a new process group, which Windows
// doesn't send the console's Ctrl+C to.
func ownProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
