// /op 监控页面与数据 API（对齐官方商业版"监控报表"的开源内存版实现）。
//
//   - GET /op           单文件监控页（HTML+CSS+原生 JS，embed 进二进制，无外部依赖）
//   - GET /op/api/stats 指标快照 JSON（group/action 维度六指标 + 小时/天序列）
//
// 默认无鉴权（开源版与 /business API 一致）；配置 OpPassword（-op-password /
// SEKIRO_OP_PASSWORD）后启用登录会话保护，见 opsign.go。
package httpapi

import (
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"sekiro-go/internal/group"
	"sekiro-go/internal/metrics"
)

//go:embed web/op.html
var opWeb embed.FS

// isOpPath /op 与 /op/api/stats 路由匹配（大小写不敏感，对齐其余 API 习惯）。
func isOpPath(path string) bool {
	return strings.EqualFold(path, "/op") || strings.EqualFold(path, "/op/")
}

func isOpStatsPath(path string) bool {
	return strings.EqualFold(path, "/op/api/stats")
}

// isOpClientsPath GET /op/api/clients：provider 节点监控信息。
func isOpClientsPath(path string) bool {
	return strings.EqualFold(path, "/op/api/clients")
}

// isOpKickPath POST /op/api/clients/kick：监控页踢下线（断开连接）。
func isOpKickPath(path string) bool {
	return strings.EqualFold(path, "/op/api/clients/kick")
}

// isOpErrorsPath GET /op/api/errors：系统错误原因分布与最近事件。
func isOpErrorsPath(path string) bool {
	return strings.EqualFold(path, "/op/api/errors")
}

// handleOpPage 输出监控页。
func handleOpPage(w http.ResponseWriter) {
	data, err := opWeb.ReadFile("web/op.html")
	if err != nil {
		httpResponseText(w, http.StatusInternalServerError, "op page missing")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// handleOpClients 输出 provider 节点监控信息（/op/api/clients）。
func handleOpClients(w http.ResponseWriter, mgr *group.Manager) {
	body, err := json.Marshal(map[string]any{"clients": mgr.ClientDetails()})
	if err != nil {
		httpResponseText(w, http.StatusInternalServerError, "marshal clients failed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// handleOpErrors 输出系统错误原因分布与最近事件（/op/api/errors）。
func handleOpErrors(w http.ResponseWriter) {
	body, err := json.Marshal(metrics.Default().Errors())
	if err != nil {
		httpResponseText(w, http.StatusInternalServerError, "marshal errors failed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// handleOpKick 监控页踢下线（POST /op/api/clients/kick）。
//
// 直接断开目标连接（触发 Disconnect 彻底注销）。/op 沿用无鉴权约定
// （内部使用，网关层有边界），但踢操作属于写操作，必须留痕：
// Warn 级记录操作来源与目标，供事后追溯。
func handleOpKick(w http.ResponseWriter, r *http.Request, mgr *group.Manager, log *slog.Logger) {
	if r.Method != http.MethodPost {
		httpResponseText(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if !requireJSONContentType(w, r) {
		return
	}
	body, err := readBodyLimited(w, r, 4<<10)
	if err != nil {
		httpResponseText(w, http.StatusBadRequest, "read body failed")
		return
	}
	var req struct {
		Group    string `json:"group"`
		ClientID string `json:"clientId"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Group == "" || req.ClientID == "" {
		httpResponseText(w, http.StatusBadRequest, `{group} and {clientId} required (json)`)
		return
	}

	ok := mgr.KickClient(req.Group, req.ClientID)
	if log != nil {
		if ok {
			log.Warn("monitor kick executed",
				"by", r.RemoteAddr, "group", req.Group, "clientId", req.ClientID)
		} else {
			log.Info("monitor kick missed (client not online)",
				"by", r.RemoteAddr, "group", req.Group, "clientId", req.ClientID)
		}
	}
	body2, _ := json.Marshal(map[string]any{"kicked": ok})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body2)))
	w.WriteHeader(http.StatusOK)
	w.Write(body2)
}

// handleOpStats 输出指标快照（在线 client 数来自 group 注册表）。
func handleOpStats(w http.ResponseWriter, mgr *group.Manager) {
	snap := metrics.Default().Snapshot(func(g string) int {
		return len(mgr.EnabledClientIDs(g))
	}, mgr.Names())
	body, err := json.Marshal(snap)
	if err != nil {
		httpResponseText(w, http.StatusInternalServerError, "marshal stats failed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// isOpLoginPath POST /op/api/login：监控路径登录（OpPassword 启用时）。
func isOpLoginPath(path string) bool {
	return strings.EqualFold(path, "/op/api/login")
}

// isOpLogoutPath POST /op/api/logout：撤销当前监控会话。
func isOpLogoutPath(path string) bool {
	return strings.EqualFold(path, "/op/api/logout")
}
