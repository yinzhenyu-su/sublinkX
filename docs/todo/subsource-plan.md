# 方案：面板内建「订阅源」，请求 mihomo 订阅时惰性刷新

> 状态：**待批准**（批准后实施，通过则移到 `docs/done/`，否则 `docs/rejected/`）
> 分支计划：`feat/subsource`（在 `main` 之上），完成后更新 fork 并可选提上游 PR

## 1. 目标

把 x-ui 的订阅地址（任意 subId，例如 `https://panel.example.com/jjsson/<subId>`）登记为
sublinkX 里的一个**订阅源（SubSource）**，绑定到某个订阅；当该订阅被客户端请求时，
若源数据已过期，则**先刷新一次再返回**，从而让 sublinkX 的 mihomo/clash 订阅自动跟随 x-ui。

不做的事：不引入面板内定时器（cron/ticker）；刷新时机 = 「订阅被请求时」。

## 2. 实测背景（源数据形态）

同一 subId 的三个路径，内容由 UA / Accept 决定：

| 路径 | `Accept: text/html`（浏览器） | mihomo / Clash 系 UA | 无 UA / Xray 系 |
|---|---|---|---|
| `/ssuubb/<subId>` | HTML 订阅页（内嵌 `__SUB_PAGE_DATA__`） | clash yaml | base64 节点链接 |
| `/clash/<subId>` | HTML 订阅页 | clash yaml | clash yaml |
| `/jjsson/<subId>` | HTML 订阅页 | Xray JSON | Xray JSON |

结论：抓取时必须带客户端 UA 且**不能带 `Accept: text/html`**，否则拿到 HTML 页而不是数据。

Xray JSON 的形态（实测）：

* 单客户端 subId → **单个配置对象**（键：`dns/inbounds/log/outbounds/policy/remarks/routing/stats`）
* 多客户端 subId → **配置对象数组**
* 每个配置里 `outbounds[].tag == "proxy"` 的那个才是真节点；`direct/blocked/block/dns_out` 跳过
* 字段示例（ws + tls）：
  `settings{address,port,id,encryption}`、
  `streamSettings{network:"ws",security:"tls",tlsSettings{alpn,fingerprint,serverName},wsSettings{host,path}}`
* 字段示例（reality + tcp）：
  `settings{address,port,id,flow:"xtls-rprx-vision"}`、
  `streamSettings{network:"tcp",security:"reality",realitySettings{publicKey,shortId,serverName,fingerprint,spiderX}}`
* `remarks` = 节点名 → 链接的 `#fragment`

即：**构建分享链接所需字段齐全**，转换是纯映射问题。

## 3. 数据模型

```go
// models/source.go（新增）
type SubSource struct {
	gorm.Model
	ID            int
	Name          string // 显示名
	URL           string // 源地址（x-ui 订阅 URL，含 subId）
	UA            string // 抓取 UA，默认 "mihomo/1.19.31"
	SubcriptionID int    // 绑定到哪个订阅（0 = 不绑定，仅入库节点）
	Group         string // 导入节点落到哪个分组，默认 "my"
	Interval      int    // 刷新间隔（秒），默认 600
	Prune         bool   // 源里消失的节点是否从订阅移除，默认 false
	Enabled       bool
	LastAt        time.Time // 上次尝试刷新时间（失败也更新，用于退避）
	LastOK        bool
	LastMsg       string // 结果/错误摘要
}
```

`models.Node` 增加一列：

```go
SourceID int // 0 = 手动添加；>0 = 来自哪个订阅源
```

去重键：`(SourceID, Link)`；同源同名但链接变化 → **更新**该节点（不新增）。

迁移：`models/sqlite.go` 的 `AutoMigrate(&User{}, &Subcription{}, &SubLogs{}, &GroupNode{}, &Node{}, &SubSource{})`
—— gorm 自动建表、自动给 `Node` 加列，无需手写 SQL。

## 4. 转换层 `node/xrayjson.go`（新增）

```go
// 返回分享链接列表；raw 支持三种形态
func DecodeSubPayload(raw []byte) ([]string, error)
```

1. 先尝试 JSON：单个对象 / 对象数组 → 每个配置转 1 条链接
2. 不是 JSON 就按 base64 解 → 按行取 `://` 开头的链接（兼容 `/ssuubb/` 形态）
3. 都不行 → 返回错误，由调用方决定是否保留旧数据

映射规则（协议 × 传输 × 安全）：

| 维度 | Xray 字段 | 分享链接参数 |
|---|---|---|
| vless | `settings.id` / `settings.flow` | `uuid` / `flow` |
| vmess | `settings.clients[0].id`（或 `settings.id`）、`settings.security` | `vmess://` base64 JSON（aid/scy/net/type/host/path/tls/sni） |
| trojan | `settings.servers[0].password` | `password` |
| ss | `settings.servers[0].method/password` | `ss://` base64 |
| ws | `wsSettings.path` / `.host` / `.headers.Host` | `type=ws&path=&host=` |
| grpc | `grpcSettings.serviceName` | `type=grpc&serviceName=` |
| xhttp | `network:"xhttp"` 或 `"splithttp"` + `xhttpSettings.path/host/mode` | `type=xhttp&path=&host=&mode=` |
| tcp+reality（含 vision） | `security:"reality"`、`realitySettings{publicKey,shortId,serverName,fingerprint,spiderX}` | `type=tcp&security=reality&pbk=&sid=&sni=&fp=&spx=` |
| tls | `tlsSettings{serverName,alpn,fingerprint,allowInsecure}` | `security=tls&sni=&alpn=&fp=&allowInsecure=` |

* 未识别的协议 / 传输：**跳过并在日志里记一行**，不让整个源刷新失败
* 节点名：`remarks` → `#url.QueryEscape(名称)`
* 生成的链接直接喂给**现有** `EncodeClash`（即已修好的转换器），不另写一份 clash 生成逻辑

## 5. 刷新流程（请求驱动，核心）

入口：订阅接口 `GET /c/?token=<md5(订阅名)>`（`routers/clients.go` 的 handler）在生成响应前调用
`source.RefreshForSubcription(sub)`：

```
1. 取该订阅绑定的 enabled 源；没有 → 原逻辑返回（零影响）
2. 快路径：now - LastAt < Interval → 直接用库里节点返回（不发网络请求）
3. 慢路径（需要刷新）：
   a. per-source mutex 加锁（并发请求只刷一次，其余等锁后走快路径）
   b. HTTP GET：UA = 源的 UA（默认 mihomo/1.19.31）、Accept: */*、超时 5s
   c. 成功 → DecodeSubPayload → 对账：
        · (SourceID, Link) 命中 → 更新
        · 同源同名、链接不同 → 更新链接
        · 未命中 → 新增（并加入该订阅的节点列表、绑定到 Group）
        · Prune=true 时删除「源里已消失且 SourceID 匹配」的节点
        · LastAt/LastOK=true/LastMsg 记录
   d. 失败 → LastAt 仍更新（退避，避免每个请求都打源）、LastOK=false、记录错误；
        **沿用库里旧节点返回**（fail-open：订阅永远不会因为源挂掉而失败）
4. 继续走原有生成流程（模板/rules/节点顺序都不变）
```

护栏（全部可调）：

* `Interval` 默认 **600s**（失败也按这个间隔退避）
* HTTP 超时 **5s**；源响应体上限 **2 MB**（防跑偏）
* 全局开关 `SubSourceEnabled`（配置项，默认 true）→ 出问题可一键停
* 刷新失败/超时**不影响**订阅响应时间之外的行为；日志写 `sublink` 标准日志（journal 可查）

## 6. API（供 Hermes / cron / 未来 UI 管理）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/source/get` | 列出源（含 LastAt/LastOK/LastMsg） |
| POST | `/api/v1/source/add` | 表单：name/url/ua/group/interval/prune/subcription |
| POST | `/api/v1/source/update` | 同上 + id |
| DELETE | `/api/v1/source/delete` | 删除源（可选是否连带删除其节点） |
| POST | `/api/v1/source/refresh` | 立即刷新（id 可选：不传=全部） |

均走现有登录态中间件。

## 7. 前端

**本次不改 UI**：`static/` 是 `webs/`（Vue + Vite，pnpm）的构建产物并被 `go:embed`，
加按钮需要引入前端构建链。先用 API 管理（Hermes/cron/curl 足够）。
若后续要 UI，单独评估 `pnpm build` 流程。

## 8. 测试与验收

见 `docs/tests/subsource-tests.md`（单元测试 + 集成测试 + 线上端到端）。

## 9. 部署与回滚

沿用已验证的流程：`go test ./...` → 交叉编译 amd64 → 备份二进制 → 替换 →
`systemctl restart sublink` → 用真实 subId 端到端验证 → 回滚一条命令。

风险点与缓解：

| 风险 | 缓解 |
|---|---|
| 请求路径里多了网络抓取 | Interval 门控 + 5s 超时 + fail-open + 全局开关 |
| 源返回 HTML（UA 不对） | 固定客户端 UA、不带 `Accept: text/html`；解析失败即保留旧数据 |
| 重复节点/节点膨胀 | `(SourceID, Link)` 去重；Prune 默认关闭 |
| 上游停更 | 补丁在自有 fork 维护（现状如此） |
| 误删节点 | Prune 默认关闭；删除前日志记录；订阅节点列表变更可回滚（DB 备份） |

## 10. 待确认（写进批准意见）

1. **源 ↔ 订阅关系**：一个源绑定一个订阅（推荐，简单）？还是源独立、订阅侧选多个源？
2. **Interval 默认值**：600s（10 分钟）可以吗？（失败退避同值）
3. **Prune 默认**：关闭（推荐）还是开启（源里删了节点，订阅里也跟着删）？
4. **导入节点分组**：源级可配，默认 `my`？
5. **刷新时机**：同步（陈旧请求最多多花 5s）vs 异步（永远立即返回旧数据、后台刷新）。
   倾向同步 + 5s 上限，行为更可预期。
