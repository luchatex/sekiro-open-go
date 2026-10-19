// Package metrics 实现 invoke 调用指标统计（对齐官方商业版监控报表的指标语义）：
//
//   - 指标维度：总量、成功量、失败量（provider 返回失败）、系统错误（服务端
//     拦截/超时）、流量（In/Out，字节）、成功率
//   - 统计粒度：group / action 两级
//   - 时间精度：小时桶（内存保留最近 48 小时，写路径惰性清理），
//     天精度在快照时由小时桶按本地时区聚合派生（对齐官方"小时累加、按天聚合"）
//
// 与官方差异：开源版无持久化存储，数据仅存内存（进程重启清零）。
package metrics

import (
	"sort"
	"sync"
	"time"
)

// Kind 调用结果分类（对齐官方指标定义）。
type Kind uint8

const (
	// Success provider 正常处理（status==0）。
	Success Kind = iota
	// Fail provider 返回业务失败（status!=0）。
	Fail
	// SysErr 服务端拦截的错误（空池/参数缺失/超时/上游连接损坏等）。
	SysErr
)

// Metrics 单维度累计值。
type Metrics struct {
	Total    int64 // 进入服务端的调用总量
	Success  int64
	Fail     int64
	SysErr   int64
	BytesIn  int64 // 请求字节（query + body / invoker payload）
	BytesOut int64 // 响应字节（协议 payload，压缩前逻辑值）
	// BytesOutWire 实际出站字节：HTTP 消费方按压缩后写入字节数实测（v1.1.3+）；
	// WS/native 消费方暂不统计（记 0），前端显示 –
	BytesOutWire int64
}

// SuccessRate 成功率（无调用时返回 0）。
func (m Metrics) SuccessRate() float64 {
	if m.Total == 0 {
		return 0
	}
	return float64(m.Success) / float64(m.Total)
}

// add2 逐字段累加。
func (m *Metrics) add2(o *Metrics) {
	m.Total += o.Total
	m.Success += o.Success
	m.Fail += o.Fail
	m.SysErr += o.SysErr
	m.BytesIn += o.BytesIn
	m.BytesOut += o.BytesOut
	m.BytesOutWire += o.BytesOutWire
}

func (m *Metrics) addResult(k Kind, bytesOut, wireOut int64) {
	switch k {
	case Success:
		m.Success++
	case Fail:
		m.Fail++
	case SysErr:
		m.SysErr++
	}
	m.BytesOut += bytesOut
	m.BytesOutWire += wireOut
}

type key struct{ group, action string }

type entity struct {
	m       Metrics
	hours   map[int64]*Metrics // unixHour（UTC）→ 该小时累计
	reasons map[string]int64   // 系统错误原因 → 计数（仅 SysErr 写入）
}

type secBucket struct {
	unix  int64 // 桶对应的 unix 秒
	total int64
}

// Recorder 指标记录器（线程安全，nil 接收者安全——埋点侧无需判空）。
type Recorder struct {
	mu      sync.Mutex
	started time.Time
	ents    map[key]*entity
	secs    [60]secBucket // 环形秒桶（最近 60s QPS）
	writes  int64         // 惰性 GC 计数

	// 系统错误原因维度（见 errors.go）：全局聚合 + 最近事件环形缓冲
	reasons map[string]*reasonStat
	ring    [errRingCap]SysErrEvent
	ringN   int64 // 累计写入数（取模定位）
}

// New 创建记录器。
func New() *Recorder {
	return &Recorder{started: time.Now(), ents: make(map[key]*entity),
		reasons: make(map[string]*reasonStat)}
}

var def = New()

// Default 进程级默认记录器（服务埋点使用；测试请用 New 独立实例）。
func Default() *Recorder { return def }

// Reset 重置默认记录器（仅测试用）。
func Reset() { def = New() }

// Invoke 调用入口：记总量、请求流量与 QPS 秒桶。
func (r *Recorder) Invoke(group, action string, bytesIn int64) {
	if r == nil || group == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entityLocked(group, action)
	h := now.Unix() / 3600
	e.m.Total++
	e.m.BytesIn += bytesIn
	e.hourLocked(h).Total++
	e.hourLocked(h).BytesIn += bytesIn
	// QPS 环形秒桶
	i := now.Unix() % 60
	if r.secs[i].unix != now.Unix() {
		r.secs[i] = secBucket{unix: now.Unix()}
	}
	r.secs[i].total++
	r.gcLocked(h)
}

// Finish 调用终结：按结果分类计数并累加响应流量。
// bytesOut 为协议 payload 逻辑字节；wireOut 为实际出站字节
//（HTTP 消费方压缩后实测，其他通道传 0）。
func (r *Recorder) Finish(group, action string, k Kind, bytesOut, wireOut int64) {
	if r == nil || group == "" {
		return
	}
	h := time.Now().Unix() / 3600
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entityLocked(group, action)
	e.m.addResult(k, bytesOut, wireOut)
	e.hourLocked(h).addResult(k, bytesOut, wireOut)
}

// AddOut 仅累加响应流量（分段流中间包：有字节产出但未终结调用）。
func (r *Recorder) AddOut(group, action string, bytesOut int64) {
	if r == nil || group == "" || bytesOut <= 0 {
		return
	}
	h := time.Now().Unix() / 3600
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entityLocked(group, action)
	e.m.BytesOut += bytesOut
	e.hourLocked(h).BytesOut += bytesOut
}

func (r *Recorder) entityLocked(group, action string) *entity {
	k := key{group, action}
	e, ok := r.ents[k]
	if !ok {
		e = &entity{hours: make(map[int64]*Metrics)}
		r.ents[k] = e
	}
	return e
}

func (e *entity) hourLocked(h int64) *Metrics {
	agg, ok := e.hours[h]
	if !ok {
		agg = &Metrics{}
		e.hours[h] = agg
	}
	return agg
}

// gcLocked 每 gcEvery 次写清理超过 48 小时的小时桶（惰性，无需独立 goroutine）。
const gcEvery = 1024

func (r *Recorder) gcLocked(nowHour int64) {
	r.writes++
	if r.writes%gcEvery != 0 {
		return
	}
	for k, e := range r.ents {
		for h := range e.hours {
			if h < nowHour-48 {
				delete(e.hours, h)
			}
		}
		if e.m.Total == 0 && len(e.hours) == 0 {
			delete(r.ents, k)
		}
	}
}

// QPS 最近 60 秒平均调用速率。
func (r *Recorder) QPS() float64 {
	if r == nil {
		return 0
	}
	now := time.Now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	var total int64
	for _, b := range r.secs {
		if b.unix > now-60 && b.unix <= now {
			total += b.total
		}
	}
	return float64(total) / 60
}

// ---- 快照输出（JSON 序列化结构） ----

// MetricJSON 单维度指标的 JSON 形态。
type MetricJSON struct {
	Total       int64   `json:"total"`
	Success     int64   `json:"success"`
	Fail        int64   `json:"fail"`
	SysErr      int64   `json:"sysErr"`
	SuccessRate float64 `json:"successRate"`
	BytesInKB   float64 `json:"bytesInKB"`
	BytesOutKB  float64 `json:"bytesOutKB"`
	// 实际出站（压缩后）KB；未统计的通道（WS/native 消费方）省略该字段
	BytesOutWireKB float64 `json:"bytesOutWireKB,omitempty"`
}

func toMetricJSON(m Metrics) MetricJSON {
	return MetricJSON{
		Total:       m.Total,
		Success:     m.Success,
		Fail:        m.Fail,
		SysErr:      m.SysErr,
		SuccessRate: m.SuccessRate(),
		BytesInKB:      float64(m.BytesIn) / 1024,
		BytesOutKB:     float64(m.BytesOut) / 1024,
		BytesOutWireKB: float64(m.BytesOutWire) / 1024,
	}
}

// ActionJSON action 维度统计。
type ActionJSON struct {
	Action  string     `json:"action"`
	Metrics MetricJSON `json:"metrics"`
}

// GroupJSON group 维度统计。
type GroupJSON struct {
	Group         string           `json:"group"`
	Clients       int              `json:"clients"` // 在线 client 数（由调用方填充）
	Metrics       MetricJSON       `json:"metrics"`
	SysErrReasons map[string]int64 `json:"sysErrReasons,omitempty"` // 系统错误原因分布
	Actions       []ActionJSON     `json:"actions"`
	Series        []int64          `json:"series"` // 最近 24 小时桶 total
}

// SeriesJSON 时间序列（顶层聚合）。
type SeriesJSON struct {
	Buckets []int64 `json:"buckets"` // 各桶起点 unix 秒
	Total   []int64 `json:"total"`
	Success []int64 `json:"success"`
	Fail    []int64 `json:"fail"`
	SysErr  []int64 `json:"sysErr"`
}

// Snapshot 全量快照。
type Snapshot struct {
	ServerTime    int64            `json:"serverTime"`
	Uptime        string           `json:"uptime"`
	QPS           float64          `json:"qps"`
	SysErrReasons map[string]int64 `json:"sysErrReasons,omitempty"` // 系统错误原因分布（全局）
	Groups        []GroupJSON      `json:"groups"`
	Hourly        *SeriesJSON      `json:"hourly"` // 最近 24 小时桶
	Daily         *SeriesJSON      `json:"daily"`  // 最近 7 天桶（本地时区）
}

// Snapshot 生成全量快照：hourly 为最近 24 个小时桶、daily 为最近 7 天桶。
// clients 参数提供各 group 在线 client 数（可为 nil）；
// registered 参数为 group 注册表中的全部 group 名（可为 nil）——
// 注册了 client 但尚无调用流量的 group 也要出现在快照中，否则监控页
// 「注册后不可见、有了请求才出现」。
func (r *Recorder) Snapshot(clients func(group string) int, registered []string) *Snapshot {
	if r == nil {
		return &Snapshot{ServerTime: time.Now().Unix(), Groups: []GroupJSON{},
			Hourly: emptySeries(0), Daily: emptySeries(0)}
	}
	now := time.Now()
	snap := &Snapshot{
		ServerTime: now.Unix(),
		Uptime:     now.Sub(r.started).Round(time.Second).String(),
		QPS:        r.QPS(),
	}

	type agg struct {
		m       Metrics
		hours   map[int64]*Metrics
		action  map[string]Metrics
		reasons map[string]int64
	}
	groups := make(map[string]*agg)

	r.mu.Lock()
	for k, e := range r.ents {
		g := groups[k.group]
		if g == nil {
			g = &agg{hours: make(map[int64]*Metrics), action: make(map[string]Metrics),
				reasons: make(map[string]int64)}
			groups[k.group] = g
		}
		g.m.add2(&e.m)
		for reason, n := range e.reasons {
			g.reasons[reason] += n
		}
		for h, m := range e.hours {
			if g.hours[h] == nil {
				g.hours[h] = &Metrics{}
			}
			g.hours[h].add2(m)
		}
		am := g.action[k.action]
		am.add2(&e.m)
		g.action[k.action] = am
	}
	r.mu.Unlock()

	hourIDs := bucketRange(now.Unix()/3600, 24) // 当前小时 + 前 23（小时序号，用于桶查询）
	dayBuckets := dayRange(now, 7)              // 天桶直接用 unix 秒

	// 注册表中尚无调用流量的 group：补进聚合（零指标），
	// 保证「client 注册后」监控页立即可见
	for _, name := range registered {
		if _, ok := groups[name]; !ok {
			groups[name] = &agg{hours: make(map[int64]*Metrics), action: make(map[string]Metrics)}
		}
	}

	gnames := make([]string, 0, len(groups))
	for name := range groups {
		gnames = append(gnames, name)
	}
	sort.Strings(gnames)

	for _, name := range gnames {
		g := groups[name]
		gj := GroupJSON{
			Group:   name,
			Metrics: toMetricJSON(g.m),
			Series:  bucketValues(g.hours, hourIDs, func(m *Metrics) int64 { return m.Total }),
		}
		if len(g.reasons) > 0 {
			gj.SysErrReasons = g.reasons
			if snap.SysErrReasons == nil {
				snap.SysErrReasons = make(map[string]int64)
			}
			for reason, n := range g.reasons {
				snap.SysErrReasons[reason] += n
			}
		}
		if clients != nil {
			gj.Clients = clients(name)
		}
		for _, a := range sortedKeys(g.action) {
			gj.Actions = append(gj.Actions, ActionJSON{
				Action:  a,
				Metrics: toMetricJSON(g.action[a]),
			})
		}
		if gj.Actions == nil {
			gj.Actions = []ActionJSON{}
		}
		snap.Groups = append(snap.Groups, gj)
	}
	if snap.Groups == nil {
		snap.Groups = []GroupJSON{}
	}

	// 顶层序列：聚合全部 group 的小时桶
	allHours := make(map[int64]*Metrics)
	for _, g := range groups {
		for h, m := range g.hours {
			if allHours[h] == nil {
				allHours[h] = &Metrics{}
			}
			allHours[h].add2(m)
		}
	}
	snap.Hourly = buildSeries(allHours, hourIDs)
	// 天桶：小时桶按本地时区归到所属天
	days := make(map[int64]*Metrics)
	for h, m := range allHours {
		d := localDayStart(time.Unix(h*3600, 0)).Unix()
		if days[d] == nil {
			days[d] = &Metrics{}
		}
		days[d].add2(m)
	}
	snap.Daily = buildSeries(days, dayBuckets)
	return snap
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func bucketRange(curHour int64, n int) []int64 {
	out := make([]int64, n)
	for i := 0; i < n; i++ {
		out[i] = curHour - int64(n-1-i)
	}
	return out
}

// dayRange 最近 n 天的"天桶"（本地时区当天 0 点 unix 秒）。
func dayRange(now time.Time, n int) []int64 {
	out := make([]int64, n)
	today := localDayStart(now)
	for i := 0; i < n; i++ {
		out[i] = today.AddDate(0, 0, -(n - 1 - i)).Unix()
	}
	return out
}

func localDayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func bucketValues(hours map[int64]*Metrics, buckets []int64, pick func(*Metrics) int64) []int64 {
	out := make([]int64, len(buckets))
	for i, b := range buckets {
		if m := hours[b]; m != nil {
			out[i] = pick(m)
		}
	}
	return out
}

// buildSeries 查询用小时序号 ids；对外 Buckets 统一输出 unix 秒（小时桶 ×3600），
// 保证与 daily 的天桶（本地 0 点 unix 秒）单位一致，前端直接 new Date(b*1000)。
func buildSeries(hours map[int64]*Metrics, ids []int64) *SeriesJSON {
	buckets := make([]int64, len(ids))
	for i, id := range ids {
		buckets[i] = id * 3600
	}
	return &SeriesJSON{
		Buckets: buckets,
		Total:   bucketValues(hours, ids, func(m *Metrics) int64 { return m.Total }),
		Success: bucketValues(hours, ids, func(m *Metrics) int64 { return m.Success }),
		Fail:    bucketValues(hours, ids, func(m *Metrics) int64 { return m.Fail }),
		SysErr:  bucketValues(hours, ids, func(m *Metrics) int64 { return m.SysErr }),
	}
}

func emptySeries(n int) *SeriesJSON {
	return &SeriesJSON{Buckets: make([]int64, n), Total: make([]int64, n),
		Success: make([]int64, n), Fail: make([]int64, n), SysErr: make([]int64, n)}
}
