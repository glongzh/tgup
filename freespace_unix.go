//go:build unix

package main

// 磁盘剩余空间。split.go 用它做「分片目录装不下」的预检。
// syscall.Statfs 仅在类 Unix 平台可用；其他平台返回 -1 表示未知、跳过检查。

import "syscall"

func freeSpace(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1
	}
	return int64(st.Bavail * uint64(st.Bsize))
}
