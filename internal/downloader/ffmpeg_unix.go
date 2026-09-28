//go:build unix

package downloader

import "syscall"

// Keep terminal signals from reaching ffmpeg.
func ownProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
