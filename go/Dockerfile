# syntax=docker/dockerfile:1
# sekiro-go 多阶段构建：最终镜像基于 alpine，约 15MB
# 构建：docker build -t sekiro-go ./go
# （构建阶段走国内代理，海外环境可用 --build-arg GOPROXY=https://proxy.golang.org,direct 覆盖）

########## 阶段 1：编译 ##########
FROM golang:1.27-alpine AS builder

ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY} \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

# 先拷贝 go.mod/go.sum 单独下载依赖，充分利用层缓存
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/sekiro ./cmd/sekiro

########## 阶段 2：运行 ##########
FROM alpine:3.20

# ca-certificates：WS/HTTPS 出站需要；tzdata：日志时区
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S sekiro \
    && adduser -S -G sekiro -h /var/lib/sekiro sekiro

COPY --from=builder /out/sekiro /usr/local/bin/sekiro

# 配置对齐 internal/config：SEKIRO_PORT（默认 5612，同原版 sekiro.port）、
# SEKIRO_LOCAL_DEBUG（调试模式）；亦可用命令行 -port/-debug 覆盖
ENV SEKIRO_PORT=5612 \
    TZ=Asia/Shanghai

EXPOSE 5612

USER sekiro

# 健康检查：groupList 为无副作用只读端点（busybox wget 内置）
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${SEKIRO_PORT}/business/groupList" >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/sekiro"]
