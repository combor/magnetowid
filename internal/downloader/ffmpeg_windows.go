package downloader

import "syscall"

// Keep console Ctrl+C from reaching ffmpeg.
func ownProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
