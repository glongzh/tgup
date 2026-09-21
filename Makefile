# tgup — 开发与发布常用命令
#
# 依赖：Go（版本见 go.mod）
# 可选：golangci-lint（make lint）、goreleaser（make snapshot / 发布）

BINARY  := tgup
PKG     := .
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# -s -w 去掉符号表和调试信息；版本信息通过 -X 注入（必须是包级字符串变量）
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

GO      ?= go
GOFLAGS ?=

.PHONY: all build install test test-race cover fmt fmt-check vet lint fuzz check snapshot clean help

all: build

## build: 编译到 ./tgup，注入版本信息
build:
	$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

## install: 安装到 $GOBIN
install:
	$(GO) install $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" $(PKG)

## test: 跑单元测试
test:
	$(GO) test -count=1 ./...

## test-race: 带竞态检测（需要 CGO 与 C 工具链；CI 在 Linux 上跑）
test-race:
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

## cover: 生成覆盖率报告并打印总覆盖率
cover:
	$(GO) test -count=1 -coverprofile=coverage.txt -covermode=atomic ./...
	$(GO) tool cover -func=coverage.txt | tail -1

## fmt: 就地格式化
fmt:
	gofmt -w .

## fmt-check: 只检查不修改（CI 用）
fmt-check:
	@test -z "$$(gofmt -l .)" || { \
		echo "以下文件未格式化，请本地执行 make fmt："; \
		gofmt -l .; \
		exit 1; \
	}

## vet: go vet
vet:
	$(GO) vet ./...

## lint: golangci-lint（需自行安装）
lint:
	golangci-lint run

## fuzz: 短时模糊测试，各 30 秒
fuzz:
	$(GO) test -run '^$$' -fuzz '^FuzzCaptionEntities$$' -fuzztime 30s .
	$(GO) test -run '^$$' -fuzz '^FuzzParseUnits$$' -fuzztime 30s .

## check: 提交前自检 = 格式 + vet + 测试（与 CI 一致）
check: fmt-check vet test

## snapshot: 本地跑一次完整发布构建，产物在 dist/（不推送）
snapshot:
	goreleaser release --snapshot --clean

## clean: 清理构建产物
clean:
	rm -f $(BINARY) $(BINARY).exe coverage.txt coverage.html
	rm -rf dist

## help: 列出全部目标
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
