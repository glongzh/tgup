package main

// 版本信息。
//
// 这里的默认值服务于本地 `go build` / `go run`；正式发布由 Makefile 与
// goreleaser 通过 -ldflags 覆盖：
//
//	go build -ldflags "\
//	  -X main.version=1.2.3 \
//	  -X main.commit=$(git rev-parse --short HEAD) \
//	  -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o tgup .
//
// 变量必须是包级 var（不能是 const），-X 只能改写字符串变量。

import "runtime"

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func goVersion() string { return runtime.Version() }
