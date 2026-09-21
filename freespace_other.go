//go:build !unix

package main

// 非 Unix 平台无法用 Statfs，返回 -1 表示未知、跳过检查。

func freeSpace(path string) int64 { return -1 }
