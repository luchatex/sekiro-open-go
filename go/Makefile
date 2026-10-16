# sekiro-go 构建脚本（Go 重写版，协议兼容 yint-tech/sekiro-open）

GO ?= go
BINARY := sekiro
PKG     := ./cmd/sekiro

.PHONY: build test race check fmt-check vet fmt clean golden

build:
	$(GO) build -o bin/$(BINARY) $(PKG)

test:
	$(GO) test ./... -count=1

# 竞态检测：覆盖 connListener 分发、group actor、Record 时序等并发路径。
# -race 需要 cgo；Windows 无 gcc 时请在 Linux/wsl 或 CI 执行（见 .github/workflows/go.yml）。
race:
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

# 提交前置检查：格式 → 静态检查 → 全量测试
check: fmt-check vet test

fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "以下文件需要 gofmt:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -rf bin

# 重新生成黄金测试向量（需要 Java + 原私有 jar，见 ../tools/golden/README.md）
golden:
	@echo "see ../tools/golden/README.md"
