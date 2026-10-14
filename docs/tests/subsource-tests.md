# 测试方案：订阅源（SubSource）与请求驱动刷新

> 配套方案：`docs/todo/subsource-plan.md`
> 原则：**先让测试失败，再实现**；每层都能独立复现。

## A. 单元测试（`go test ./...`，随代码进 fork/PR）

### A1. 源载荷解析 `node/xrayjson_test.go`

用 fixture 覆盖三种载荷形态与各协议/传输组合，断言生成的分享链接可被**现有** `Decode*URL` 解析回
等价字段（往返一致性），而不是只比对字符串。

| 用例 | 输入 | 断言 |
|---|---|---|
| ws+tls 单对象 | 实测的 `<subId-A>` JSON（单元素数组） | 2 条链接；`type=ws`、`security=tls`、`sni`、`alpn`、`path=/ws`、`#名称` |
| reality+tcp 单对象 | 实测的 `<subId-B>` JSON | `security=reality`、`pbk`、`sid`、`fp=chrome`、`spx`、`flow=xtls-rprx-vision` |
| xhttp | 构造 `network:"xhttp"` + `xhttpSettings{path,host,mode}` | `type=xhttp&path=&host=&mode=`，且 `EncodeClash` 产出 `xhttp-opts`（回归上次的修复） |
| grpc | `network:"grpc"` + `grpcSettings.serviceName` | `type=grpc&serviceName=` |
| vmess | vmess 配置 | `vmess://` base64，字段可被 `DecodeVMESSURL` 还原 |
| trojan / ss | 各自配置 | 链接可被对应 `Decode*URL` 还原 |
| **base64 形态** | `/ssuubb/` 的 base64 文本 | 按行取出全部链接 |
| 混合/异常 | 空、非 JSON 非 base64、含未知协议、含 `direct/blocked` outbound | 不 panic；跳过未知；错误可返回 |
| 名称转义 | `remarks` 含空格/中文/`#` | `#fragment` 正确转义，`Decode` 后名称一致 |

### A2. 源同步对账 `models/source_test.go`（或 `node/` 下等价的纯函数 + DB 内存库）

* 新增：库里空 → 全量入库，`SourceID` 正确，节点绑到指定分组
* 幂等：同一载荷连刷两次 → 节点数不变（不重复）
* 更新：同名不同链接 → 更新现有节点，不新增
* 消失：默认（Prune=false）→ 保留；Prune=true → 删除该源消失的节点
* 不误伤：手动节点（`SourceID=0`）与其它源的节点不受影响
* 订阅同步：新节点自动进入绑定订阅的节点列表；手动加入订阅的节点不被移除

### A3. 刷新门控与护栏

* 计划内间隔：`LastAt` 在 Interval 内 → **不发网络请求**（用可注入的 http client / fake source 计数断言 0 次）
* 失败退避：源返回 500 / 超时 / HTML 页面 → `LastOK=false`、`LastAt` 仍推进、**库里旧节点照常返回**
* 并发：10 个 goroutine 同时请求同一订阅 → 实际抓取次数 = 1
* 全局开关关闭 → 完全不刷新

## B. 集成测试（本地跑真实面板副本）

沿用已验证的本地预览法（不改线上）：

```bash
mkdir -p sublink-test && ssh ladder 'tar -C /home/yin -czf - .sublink' | tar -xzf - -C sublink-test
cd sublink-test/.sublink && /path/sublink-<arch> run --port 8002
```

1. `POST /api/v1/source/add` 登记实测 subId（`/jjsson/<subId-A>`），绑定某个测试订阅
2. `GET http://127.0.0.1:8002/c/?token=<md5(测试订阅)> -A "mihomo/1.19.31"` → 返回的 yaml 里出现
   源内节点，且字段正确（ws → `ws-opts`，reality → `reality-opts`）
3. 再请求一次（Interval 内）→ 面板日志**没有**第二次抓取记录（门控生效）
4. `POST /api/v1/source/refresh` 强制刷新 → 日志有抓取、`LastOK=true`
5. 把源的 URL 改成一个必然失败的地址（如 `https://127.0.0.1:1/x`）→ 订阅仍 200、节点仍在、`LastMsg` 有错误
6. 恢复 URL → 一分钟后再次请求 → 恢复正常

## C. 线上端到端验收（部署后，Pi 侧）

1. 部署：备份二进制 → 替换 → `systemctl restart sublink` → 面板 200
2. Pi：`clashsub update` → `runtime.yaml` 里出现来自该源的节点；`mihomo` API 打延迟全绿
3. 在 x-ui 侧**改动一个节点的参数**（例如给某个客户端换个 ws path）→ 等过 Interval → Pi 再拉订阅 →
   新参数生效（证明"跟着 x-ui 走"闭环成立）
4. 回滚演练：`/usr/local/bin/sublink/sublink.bak-<ts>` 换回 + restart → 面板与订阅恢复正常

## D. 回归清单（每次改动都跑）

* 现有 7 节点订阅输出不变（`ws-opts` / `reality-opts` / `xhttp-opts` 各就各位）
* 模板规则条数不变（当前 34 条）
* base64 形态订阅仍返回全部链接
* `go test ./node/ -run TestClashTransportOpts`（上次的修复回归）
* 请求订阅的响应时间：Interval 内与改动前一致（± 少量 DB 开销）

## E. 判定标准

* A/B 全绿 + C 端到端跑通 + D 无回归 → 认为通过，文档移至 `docs/done/`
* 任一环节失败 → 记录到 `docs/rejected/` 并附失败输出

---

## F. 集成测试结果（本地副本实例，2026-10-14）

在 Pi 上用线上 DB 的只读副本跑补丁版二进制（`:8002`，`systemd --user` 临时单元），
`source_it_v2.py` 覆盖 9 组场景，**18/18 通过**：

| # | 场景 | 结果 |
|---|------|------|
| 1 | 登记源 → 请求订阅触发抓取（`LastAt` 前进） | ✅ |
| 2 | 源节点归属该源、出现在订阅输出、无重名 | ✅ |
| 3 | 间隔内再请求 → 零抓取（`LastAt` 不变）、内容一致 | ✅ |
| 4 | `source/refresh` 强制刷新 | ✅ |
| 5 | 源地址不可达 → 订阅仍 200、旧节点保留、状态标失败（fail-open） | ✅ |
| 6 | 恢复地址 → 恢复正常 | ✅ |
| 7 | 其它 3 个订阅（sublink-x / frontend-server / 共享）无回归 | ✅ |
| 8 | 删源（delNodes=true）→ 接管来的原有节点不被误删 | ✅ |

**踩坑记录（写进 skill）**

* 请求订阅做验证时必须用 **clash 系 UA**（如 `clash-verge/v2.0.0`）。
  `mihomo/1.19.31` 不含 `clash` → 面板按 UA 分发返回 base64，断言 clash 格式会假失败。
* 本地实例用 `systemd-run --user --unit=... --collect` 起，日志看 `journalctl --user -u ...`；
  用 `&`/`nohup`/`setsid` 起的后台进程在本机环境下会被回收。
* 记 curl 前先 `unset http_proxy https_proxy`，否则 localhost 请求会走 mihomo 代理返回 502。
