// sekiro-go：yint-tech/sekiro-open 的 Go 重写版服务端。
// 线上协议与原 Java 实现字节级兼容（见 docs/go-rewrite-dev-plan.md）。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"sekiro-go/internal/config"
	"sekiro-go/internal/server"
)

func main() {
	cfg := config.Load()
	// 日志级别优先级：-debug > -log-level > 默认 Info。
	// -debug 额外输出完整调用参数；-log-level warn/error 可把逐笔调用记录
	// 关掉（内部部署监控页已可查调用，无需落盘），避免日志文件快速增长
	level, levelOK := cfg.SlogLevel()
	if !levelOK {
		fmt.Fprintf(os.Stderr, "invalid -log-level %q, falling back to info\n", cfg.LogLevel)
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h))
	slog.Info("welcome use sekiro framework(go), for more support please visit our website: https://iinti.cn/")

	srv, err := server.New(cfg)
	if err != nil {
		slog.Error("sekiro server start failed", "err", err)
		os.Exit(1)
	}

	// SIGINT/SIGTERM → 优雅关停：停止接入新连接、等待在途调用回包后再退出
	// （容器 stop / systemd stop 均走 SIGTERM，此前无处理即直接杀进程）
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx); err != nil {
		slog.Error("sekiro server exited", "err", err)
		os.Exit(1)
	}
	slog.Info("sekiro server(go) stopped gracefully")
}
