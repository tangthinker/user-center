# user-center 认证体系重构设计

> **状态**：v2.0 · 待评审
> **范围**：`github.com/tangthinker/user-center` 及其下游消费方 `github.com/tangthinker/cloud-core`
> **原则**：本文档只描述设计。落地施工时以本文档的"不变量"和"验收清单"为验收依据。

---

## 修订记录

| 版本 | 变更 |
|---|---|
| v1.0 | 初稿：17 条设计决策、总体架构、数据模型、业务流程、安全设计、邮件子系统、admin 子系统、上线 runbook、32 条验收清单 |
| v1.1 | 邮件通道由网易邮箱改为**阿里企业邮箱（免费版，自有域名已绑定）**；新增 §7.2「配额保护与发送优先级」；补齐阿里 SMTP 参数、**三方客户端权限前置条件**与 SPF/DKIM/DMARC 清单；新增 `UC_MAIL_DAILY_QUOTA` 环境变量 |
| v1.2 | 按"个位数用户"规模精简 §7.2：删去优先级队列、配额预留、自适应节流、多级告警，只保留 6 条必要措施；补上"最大可被烧掉的量 = 单邮箱日上限 × 用户数"的上界推导 |
| v2.1 | 同步实现状态：新增「实现状态（v2.0.0）」小节记录 11 项实现偏差与推迟项 |
| v2.0 | **按"依赖库"形态重构**：新增 §0 形态约束（10 条不变量 + 3 个现存缺陷）、§12 版本化与兼容策略；改写 §3.1 挂载模型、§3.2 每宿主存储、§7.2 多宿主配额共享、§8.2 配置驱动 bootstrap、§9 每宿主 runbook；新增决策 #18–#25 |

---

## 实现状态（v2.0.0）

**状态**：已实现，`go test ./... -race` 全绿（14 个包 / 168+ 用例）。
代码入口见 [`README.md`](../README.md)，逐阶段决策与偏差见 [`dev-log/v2.0.0.md`](dev-log/v2.0.0.md)。

### 与本文档设计的偏差（实现时确认的调整）

| # | 设计文档 | 实现 | 原因 |
|---|---|---|---|
| 1 | 模块路径不变 | `github.com/tangthinker/user-center/v2` | Go modules 对主版本 ≥2 的硬性要求；宿主需改 import 行 |
| 2 | admin 独立二进制 | admin Handler 由宿主挂到**第二个回环监听** | 库无法 `Listen`（L1）；`localhost` 仍需 `LocalOnly` 兜底 |
| 3 | SMTP 由库内配置 | `Config.Mailer` 接口注入，SMTP 只是默认实现 | 宿主可换任意 provider（L4/L6） |
| 4 | 邮件"事务提交后入队" | 入队放在**业务事务内**（outbox 模式） | 提交则邮件必在队列、回滚则无幽灵邮件，崩溃窗口更小 |
| 5 | 未提 | 发送成功后**清空 outbox.payload**；终态失败同样清空 | 邀请链接与验证码明文不应长期留存 |
| 6 | `invitations` 无 check_count | 新增 `check_count` 列 | `check-uid` 的每 token 次数限制需要原子计数 |
| 7 | 设计提到"软删除陷阱" | 完全不使用软删除（无 `deleted_at`） | 从根上规避"被删用户永久占住邮箱" |
| 8 | 未提 | 未配置 Mailer 时**不入队**且 `mail_queued=false` | 诚实语义：避免管理员以为邮件已发出 |
| 9 | IP 维度也可落库 | IP 维度是**进程内窗口**，全局熔断落库兜底 | 避免高频攻击把写压力放大到 SQLite；重启清零可接受 |
| 10 | — | `fiber` 依赖取 v2.52.15 | 含后续安全补丁；宿主升级时会随 MVS 取到 |
| 11 | — | 前置检查用 `bool` 返回而非 `error` | Fiber 的处理器链以"返回 nil"表示继续，写完响应再 return nil 会被放行到业务处理器（实测踩到） |

### 明确推迟的内容

TOTP / Passkey / API Key（机器凭据）/ 跨宿主身份打通 / 多副本与 Redis /
邮件优先级队列与配额预留 / 60-80-90 多级告警 / 退信自动解析。
触发条件与理由见 §11 与 §7.2 的"本期不做"小节。

---

## 目录

- [修订记录](#修订记录)
- [0. 形态约束（本库寄生在宿主服务内）](#0-形态约束本库寄生在宿主服务内)
- [1. 背景与目标](#1-背景与目标)
- [2. 设计决策记录](#2-设计决策记录)
- [3. 总体架构](#3-总体架构)
- [4. 数据模型](#4-数据模型)
- [5. 业务流程](#5-业务流程)
- [6. 安全设计](#6-安全设计)
- [7. 邮件子系统](#7-邮件子系统)
- [8. admin 子系统](#8-admin-子系统)
- [9. 上线与回滚 runbook](#9-上线与回滚-runbook)
- [10. 验收测试清单](#10-验收测试清单)
- [11. 未决问题](#11-未决问题)
- [12. 版本化与兼容策略](#12-版本化与兼容策略库形态的硬要求)
- [附录 A：与现状代码的差异清单](#附录-a与现状代码的差异清单)

---

## 0. 形态约束（本库寄生在宿主服务内）

**本节是本文档的最高优先级约束。凡与之冲突的章节，一律以本节为准。**

`user-center` 是一个**被上游服务以 tag 版本引用的 Go 依赖库**，不是独立部署的服务。它寄生在宿主进程内部，并且**每个宿主服务都拥有自己的用户集合与自己的管理员**。

已核实的事实：

- 已知宿主：`cloud-core`，其 `v1.1.6` 与 `v1.1.7` **都锁在 user-center v1.0.7**；库已发布的 tag 为 `v1.0.7`、`v1.2.6`。
- 当前公开 API：`pkg.RegisterUserCenter(router fiber.Router, userDBRootPth string)` 与 `pkg.TokenValid(token string) (string, error)`（`pkg/fiber_adapter.go:10,25`），宿主通过 `ctx.Locals("uid")` 取用户身份（`cloud-core@v1.1.7/pkg/export.go:24,30`）。

### 0.1 由形态决定的不变量

| # | 约束 | 直接后果 |
|---|---|---|
| L1 | 库**不能**选择监听地址与端口 | 库只能提供 Handler/Router，由宿主挂载 ⇒ "admin 独立进程"不成立 |
| L2 | 库状态必须**按实例隔离** | 禁止进程级全局单例；**一个库实例 = 一个用户域**（要两套就 `New()` 两次） |
| L3 | 库 panic = **宿主进程崩溃** | 所有初始化与运行时路径返回 error；禁止 `panic` / `log.Fatal` / `os.Exit` |
| L4 | 库配置来自**宿主注入** | 不读全局 env；用 `Config` 结构体，来源由宿主决定 |
| L5 | 告警/日志/指标去向由**宿主**决定 | 用 Hooks 回调暴露；库不自己发告警邮件、不自己写日志文件 |
| L6 | 每个宿主按自己的节奏升级 tag | 同主版本内 schema **只做加法**；旧版库遇到新 schema 必须**拒绝启动** |
| L7 | 已发布 tag 不可修改 | 修 bug 必须发新 tag；破坏性 API 变更必须升主版本（v2） |
| L8 | 库的生命周期由宿主决定 | 库启动的 goroutine/句柄必须由 `Close()` 回收（现状 `internal/service/auth/pebble.go:43-52` 的消费者 goroutine 无法回收） |
| L9 | 每个宿主独立的 DB / 用户 / 管理员 | 无中心视图；审计按宿主隔离；运维步骤 × 宿主数量 |
| L10 | 邮件通道可能被多宿主共享 | 共用同一发件邮箱时，**配额与发信信誉是共享的**（§7.2） |

### 0.2 本文档中受此约束改写的章节

| 章节 | 原（服务形态）假设 | 改写后 |
|---|---|---|
| §3.1 | 库自己起进程与端口 | 宿主内嵌 + 宿主自行启动第二个回环监听 |
| §3.2 | 全系统唯一存储 | **每个宿主各自的**存储 |
| §7.2 | 全局 200 封/天配额 | 每宿主配额；共用发件账号时配额共享 |
| §8.2 | `admin serve --init-admin` 命令 | `Config.BootstrapAdminEmail`，配置驱动、幂等 |
| §9 | 单服务停机重建 | 每宿主各自升级 tag + 首启自动迁移 |
| §12（新增） | — | 版本化与 schema 兼容策略 |

### 0.3 由此暴露的三个现存缺陷（库形态特有）

1. **全局单例 + `sync.Once`**（`internal/db/sqlite.go:10-32`、`internal/service/auth/pebble.go:22-25`）：
   - 一个进程只能有一个用户域，做不了两套用户；测试之间互相污染（`internal/service/auth/memory_test.go` 甚至依赖这种全局共享）。
   - `SetDBPath` 在首次 `GetDB`/`GetPebbleAuth` 之后调用**永久失效**。
   - **真实隐患**：若 `pkg.TokenValid` 先于 `RegisterUserCenter` 被调用，Pebble 会以**进程 CWD** 打开 `pebble-token-auth.db`（此时 `GetDBPath()` 仍为空串，见 `internal/service/auth/pebble.go:29-33`），之后 `SetDBPath` 再也改不回来 ⇒ **令牌库与用户库落在两个不同目录**。cloud-core 只是恰好因为调用顺序（`main.go:67` 先注册、请求时才校验）没踩到。
2. **库的初始化路径会 panic**（`internal/db/sqlite.go:25`、`internal/service/auth/pebble.go:36`）：库 panic 会带走宿主进程；且 `sync.Once` 在 panic 后仍标记完成 ⇒ 后续调用拿到 `nil`。
3. **隐式副作用不可控**：`RegisterUserCenter` 一调用就 `AutoMigrate` 建表（`internal/model/user.go:19`），宿主无法控制时机、无法 dry-run、无法在只读场景下避免写入。

---

## 1. 背景与目标

### 1.1 现状问题（审计结论摘要）

| 编号 | 问题 | 证据 |
|---|---|---|
| P0-1 | 注册闸门是硬编码常量，且随公开 Go module 一起分发 | `internal/api/manager/manager.go:64,96`；`req/req.http:14` |
| P0-2 | 口令存储为**无盐 MD5**，无 work factor | `internal/helper/pwdencry/encrypt.go:20-30` |
| P0-3 | 登录/改密**零限流、零锁定、零 IP 封禁、零审计** | `main.go:11`（无任何中间件）；仓库内 `grep "\.Use("` 零命中 |
| P0-4 | 会话**不可吊销**，`Auth` 接口只有 `Sign/Verify` | `internal/service/auth/interface.go:3-6` |
| P1-1 | 会话令牌**明文落盘** + 滑动续期等于永不过期 | `internal/service/auth/pebble.go:95,143-148` |
| P1-2 | 硬编码 JWT 密钥 + 未被使用的 `JWTAuth`（HS256） | `internal/constrant/auth.go:7`；`internal/service/auth/jwt.go:12-16` |
| P1-3 | 用户枚举：`/uid-unique` 完全公开；登录错误信息可区分 | `main.go:21`；`internal/service/manager/manager.go:42,51` |
| P1-4 | 认证失败返回 **HTTP 200**（body 里写 code），爆破在监控上不可见 | `internal/helper/response/filber.go:19-24` |
| P2 | 无口令策略、无审计、库初始化 panic、`sync.Once` panic 后返回 nil | `internal/db/sqlite.go:25`；`internal/service/auth/pebble.go:36` |

### 1.2 目标

1. **消除口令攻击面**：全系统不再存储任何用户口令。
2. **消除公开注册面**：用户只能由管理员创建，且必须通过邮箱邀请链接完成激活。
3. **会话可控**：可吊销、有绝对有效期、服务端只存令牌哈希。
4. **限流与封禁可用**：多维度限流、429 语义正确、异常可观测、可被封禁。
5. **管理面不出网**：admin 只绑定回环地址，公网无法触达——且这一约束必须由**库侧的 `LocalOnly` 中间件**兜底，因为库无法控制宿主怎么挂载（§3.1）。
6. **下游无感**：`pkg.TokenValid(token) (string, error)` 的签名与语义保持不变，`cloud-core` 不改代码即可升级。
7. **不越界**（库形态新增）：不 panic、不接管日志、不读全局 env、不起无法回收的 goroutine。
8. **可多实例**（库形态新增）：同一宿主进程内可创建多个互相隔离的实例；测试用临时目录即可完全隔离。

### 1.3 非目标

- 不引入 MFA / TOTP / Passkey（本期）。
- 不引入 API Key / 机器凭据（已确认无机器客户端）。
- 不做多副本 / HA（已确认永远单实例）。
- 不做数据迁移（已确认清空重建）。

---

## 2. 设计决策记录

| # | 决策 | 选择 | 理由 | 状态 |
|---|---|---|---|---|
| 1 | 登录方式 | 纯邮箱一次性验证码，零口令 | 消除口令库爆破与撞库；无口令可泄漏 | ✅ 已确认 |
| 2 | 用户来源 | 仅 admin 邀请制 | 消灭公开注册面与 `loveVG` 类共享密钥 | ✅ 已确认 |
| 3 | 激活流程 | 点邀请链接后由**用户自己设置 uid**，设置完成才激活 | uid 是用户可见身份，交给用户；管理员只负责发邀请 | ✅ 已确认 |
| 4 | 邀请 TTL | **7 天** | 兼顾可达性与暴露窗口 | ✅ 已确认 |
| 5 | 邀请 token | 32 字节随机 / 只存哈希 / 单次消费 / **GET 不消费** | 防邮件网关预取把链接烧掉 | ✅ 已确认 |
| 6 | 激活后是否发放会话 | **不发放**，统一走 OTP 登录 | 发会话路径唯一 = bug 唯一；转发邮件不等于拿到会话 | ✅ 已确认 |
| 7 | admin 认证 | 邮箱 OTP（**不加 TOTP**）+ 仅本地访问 | 本地绑定即单因子的补偿控制 | ✅ 已确认 |
| 8 | admin 界面 | 静态 HTML（无 TUI、无构建工具） | 无供应链风险、可控 CSP、运维简单 | ✅ 已确认 |
| 9 | admin 复制邀请链接 | 提供该按钮，语义为**重新生成**（旧链接立即失效） | 只存哈希 ⇒ 明文无法二次取回 | ✅ 已确认 |
| 10 | 欢迎邮件 | 激活成功 + 首次登录，各一封 | 确认可达性 + 提升体验 | ✅ 已确认 |
| 11 | 改邮箱 | **仅 admin** 可改 | 邮箱是唯一凭据，收紧写路径 | ✅ 已确认 |
| 12 | 存储 | 唯一 **SQLite(WAL)**，Pebble 退役 | TTL、二级索引、跨进程吊销、备份、去重依赖 | ✅ 已确认 |
| 13 | 存量用户 | **清空重建**，不备份 uid 数据 | 无口令面残留、无双跑窗口 | ✅ 已确认 |
| 14 | 邮件通道 | **阿里企业邮箱（免费版）SMTP**，自有域名已绑定 | 自有域名 ⇒ SPF/DKIM/DMARC 可自行配置，到达率可控；SMTP 已是抽象，可随时换 provider | ✅ 已确认 |
| 15 | uid 规则 | **大小写敏感** / 最少 3 字符 / 不能数字打头 | 用户自选身份，需明确规则 | ✅ 已确认（字符集细节见 §5.2） |
| 16 | 会话生命周期 | idle 7 天 + absolute 30 天，续期不破上限 | 终结"用一次活 15 天" | ⏳ 待确认（可用默认值） |
| 17 | 部署形态 | **每个宿主进程单实例**；库可被同一进程多次实例化 | 已确认永远单实例；库形态要求状态按实例隔离（§0.1 L2） | ✅ 已确认 |
| 18 | 公开 API | `New(Config) (UserCenter, error)` 实例化，取代全局单例 | L2/L4：库不能占用进程级全局状态、不能读全局 env | ✅ 已确认 |
| 19 | 兼容垫片 | 保留 `RegisterUserCenter` / `TokenValid` 作为"默认实例"薄封装 | L7：cloud-core 锁在 v1.0.7，不做代码改动即可升级 | ⏳ 待确认 |
| 20 | admin 挂载 | 库提供 Handler，由宿主挂到第二个回环监听 | L1：库无法自己 Listen | ✅ 已确认 |
| 21 | admin 本地判定 | **只认 `RemoteAddr`**，非回环一律 403（fail-closed） | 库无法控制宿主的代理信任配置（§3.1、§6.4） | ✅ 已确认 |
| 22 | admin bootstrap | `Config.BootstrapAdminEmail`，幂等 | L9：每个宿主自己的 admin，配置驱动而非全局命令 | ✅ 已确认 |
| 23 | 邮件发送 | `Mailer` 接口由宿主注入，库内置 SMTP 默认实现 | L4：宿主可换 provider；库不读 env | ✅ 已确认 |
| 24 | 可观测性 | Hooks 回调（邮件失败 / 限流 / 管理动作） | L5：告警去向由宿主决定 | ✅ 已确认 |
| 25 | schema 兼容 | 同主版本只做加法；旧版库遇新 schema **拒绝启动** | L6/L7：防跨版本数据损坏（§12.2） | ✅ 已确认 |

---

## 3. 总体架构

### 3.1 挂载模型（库形态）

```
宿主服务进程（如 cloud-core）
├── 公共监听 :9999  ◄── Caddy ◄── Internet
│     └── 业务路由 + uc 公共面：/api/v1/{invite,otp,session}*
└── 回环监听 127.0.0.1:9998（宿主另行启动，仅本机可达）
      └── uc.AdminHandler()：静态 UI + JSON API + 审计

每个宿主进程内部：一个 uc 实例 → 一份 SQLite(WAL) → 一套用户 + 自己的 admin
N 个宿主 = N 个独立实例、N 份 DB、N 套用户与 admin
```

**库的职责**：提供公共面与 admin 面的 Handler、自己的存储与迁移、`embed` 的静态 UI。
**宿主的职责**：决定监听地址与端口、决定 admin 面挂在哪个监听上、注入 `Config` 与 `Mailer`、决定日志与告警去向。

**"admin 只允许本地访问"在此模型下如何成立**（两道防线，缺一不可）：

1. **宿主侧**：把 admin Handler 挂到**只绑 `127.0.0.1` 的第二个监听**上（文档提供 3 行示例），并确保 Caddy 不代理该端口。
2. **库侧（硬兜底）**：`AdminHandler` 内置 `LocalOnly` 中间件，判定**只认 `RemoteAddr`**（`127.0.0.1` / `::1`），非回环一律 403。即使宿主把 admin 路由误挂到公网监听上，外部请求也进不来。

⚠ **为什么"只认 `RemoteAddr`"是硬要求**：库运行在宿主进程内，`c.IP()` 的取值取决于**宿主的 Fiber 代理配置**（`EnableTrustedProxyCheck` / `ProxyHeader`），库既无法控制也无法校验。若用 `c.IP()` 做本地判定，任何配了 `ProxyHeader` 却没开 `EnableTrustedProxyCheck` 的宿主，都会被 `X-Forwarded-For: 127.0.0.1` 直接穿透（详见 §6.4）。

⚠ **`LocalOnly` 的已知边界（必须写进宿主接入文档）**：如果宿主在回环之上又加了一层**本机反向代理**（例如 nginx 把 `/admin` 代理到 `127.0.0.1:9998`），那么所有请求的 `RemoteAddr` 都是 `127.0.0.1`，`LocalOnly` 会失效。因此：

1. **admin 监听之前不要加任何反向代理**（Caddy 必须显式拒绝该端口/路径）；
2. 库侧再叠加第三层校验：**`Host` 头白名单**，只接受 `127.0.0.1:<admin端口>` 与 `localhost:<admin端口>`（同时挡 DNS rebinding）。

### 3.2 存储（每个宿主一份）

- **每个宿主进程一份** `user-center.db`（SQLite，WAL），由 `Config.DBPath` 指定。库内不再有全局 `SetDBPath`（该设计在库形态下必然出错，见 §0.3 第 1 条）。
- **Pebble 退役**：旧 `pebble-token-auth.db` 直接删除（其存在意义就是"全部会话作废"）。
- **多宿主之间不共享任何存储**：不共享 DB、不共享令牌、不共享管理员；跨宿主的身份打通不在本期范围内。

DSN（`mattn/go-sqlite3`，参数已核实支持）：

```
file:user-center.db?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_foreign_keys=on&_txlock=immediate
```

约束与注意：

| 项 | 说明 |
|---|---|
| WAL 前提 | 需同机共享内存，**不能放 NFS/网络盘** |
| 单写者 | SQLite 只允许一个写事务；所有事务必须短，**禁止在事务内发邮件/发 HTTP** |
| `_txlock=immediate` | 让写事务立即取写锁，避免升级锁导致的 `SQLITE_BUSY` |
| `busy_timeout` | 驱动默认 5000ms（`sqlite3.go:1097,1490`），保持 |
| 驱动 | `mattn/go-sqlite3` 依赖 cgo；如需纯 Go 可换 `modernc.org/sqlite`（本期不改） |
| 文件权限 | 含 PII，目录与文件权限收敛到服务账号，`0600` |
| 迁移 | 用显式版本化迁移替换 GORM `AutoMigrate`（见 §4.4） |

### 3.3 路由总表

**公开面**（`public :9999`，经 Caddy）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/invite?token=…` | 邀请落地页（静态 HTML）。**只渲染，不改变任何状态** |
| POST | `/api/v1/invite/check-uid` | `{token, uid}` → `{available}`；token 限定 + 限次 |
| POST | `/api/v1/invite/accept` | `{token, uid}` → 消费 token、设置 uid、激活 |
| POST | `/api/v1/otp/send` | `{email}` → 统一响应 |
| POST | `/api/v1/otp/verify` | `{email, code}` → `{token, uid}` |
| POST | `/api/v1/session/verify` | `{token}` → `{uid}`（兼容 cloud-core 现有语义） |
| POST | `/api/v1/session/logout` | `{token}` → 吊销当前会话 |

**admin 面**（`127.0.0.1:9998`，Caddy 不代理）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` `/assets/*` | 静态 HTML/CSS/JS（同源，无 CDN） |
| POST | `/admin/otp/send` | 仅允许管理员邮箱 |
| POST | `/admin/otp/verify` | 换 admin 会话 |
| POST | `/admin/logout` | 吊销当前 admin 会话 |
| GET | `/admin/users` | 列表（含 status、是否待激活、last_login_at、`online_devices` 在线设备数） |
| POST | `/admin/users` | `{email}` → 建用户（`invited`）+ 发邀请 |
| POST | `/admin/users/:id/invite/resend` | 重发（作废旧 token） |
| POST | `/admin/users/:id/invite/link` | **重新生成并返回明文链接一次**（供复制） |
| POST | `/admin/users/:id/disable` / `enable` | 停用/启用 |
| POST | `/admin/users/:id/email` | 改邮箱（通知旧地址 + 吊销全会话） |
| POST | `/admin/users/:id/sessions/revoke` | 踢掉该用户全部会话 |
| GET | `/admin/users/:id/sessions` | 该用户的**在线设备**（按设备指纹归并：类型/型号/系统/浏览器/登入时间/最近活跃/IP） |
| DELETE | `/admin/users/:id` | 删除用户 |
| GET | `/admin/audit` | 审计查询 |

### 3.4 模块划分

**删除**（详见附录 A）：`internal/helper/pwdencry/`、`internal/helper/idgen/`、`internal/service/auth/{jwt,memory,pebble,common}.go`、`internal/constrant/auth.go`。

**新增**：

```
usercenter.go                 ← 新的公开入口：Config / New / Close / UC 接口
internal/store/               纯数据层（不依赖 fiber，按实例持有 *gorm.DB）
internal/mail/                Mailer 接口 + 内置 SMTP 实现 + outbox 队列 + 模板
internal/ratelimit/           多维度计数
internal/otp/                 验证码签发与校验
internal/invite/              邀请签发、校验、消费
internal/session/             会话签发、校验、续期、吊销
internal/audit/               审计写入
internal/adminui/             embed.FS 静态 UI + admin JSON API + LocalOnly 中间件
internal/migrate/             显式版本化迁移（在 New() 内执行）
legacy.go                     v1 兼容垫片：RegisterUserCenter / TokenValid（默认实例）
```

**不再新增 `cmd/admin/`**：库形态下没有独立 admin 进程，admin 面由宿主挂到自己的回环监听上（§3.1）。

---

## 4. 数据模型

### 4.1 DDL

```sql
-- ---------- 用户 ----------
CREATE TABLE users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,   -- 内部主键（邀请期的身份载体）
  uid           TEXT    NULL,                        -- 激活时由用户设定，之后不可变
  email         TEXT    NOT NULL,                    -- 归一化：trim + 转小写
  status        TEXT    NOT NULL,                    -- invited | active | disabled
  is_admin      INTEGER NOT NULL DEFAULT 0,
  activated_at  TIMESTAMP NULL,
  last_login_at TIMESTAMP NULL,
  disabled_at   TIMESTAMP NULL,
  created_by    INTEGER NULL,                        -- 操作者 users.id
  created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- email 唯一：存归一化小写值 ⇒ 效果上大小写不敏感
CREATE UNIQUE INDEX ux_users_email ON users(email);
-- uid 唯一且【大小写敏感】：SQLite 默认 BINARY collation
CREATE UNIQUE INDEX ux_users_uid ON users(uid) WHERE uid IS NOT NULL;

-- ---------- 邀请 ----------
CREATE TABLE invitations (
  token_hash   BLOB PRIMARY KEY,                     -- sha256(token)，明文永不落库
  user_id      INTEGER NOT NULL REFERENCES users(id),
  email        TEXT NOT NULL,
  purpose      TEXT NOT NULL,                        -- invite | email_change
  issued_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at   TIMESTAMP NOT NULL,
  consumed_at  TIMESTAMP NULL,
  created_by   INTEGER NOT NULL,
  sent_count   INTEGER NOT NULL DEFAULT 0,
  last_sent_at TIMESTAMP NULL
);
CREATE INDEX ix_invitations_user    ON invitations(user_id);
CREATE INDEX ix_invitations_expires ON invitations(expires_at);

-- ---------- 一次性验证码 ----------
CREATE TABLE otp_codes (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  email       TEXT NOT NULL,
  purpose     TEXT NOT NULL,                         -- login | admin_login
  code_hmac   BLOB NOT NULL,                         -- HMAC-SHA256(code, server_key)
  issued_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at  TIMESTAMP NOT NULL,
  attempts    INTEGER NOT NULL DEFAULT 0,
  consumed_at TIMESTAMP NULL,
  request_ip  TEXT NULL
);
-- 同一 (email, purpose) 同时只能有一个有效码
CREATE UNIQUE INDEX ux_otp_active ON otp_codes(email, purpose) WHERE consumed_at IS NULL;

-- ---------- 会话 ----------
CREATE TABLE sessions (
  token_hash          BLOB PRIMARY KEY,              -- sha256(token)
  user_id             INTEGER NOT NULL REFERENCES users(id),
  uid                 TEXT NOT NULL,
  scope               TEXT NOT NULL,                 -- user | admin
  issued_at           TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  idle_expires_at     TIMESTAMP NOT NULL,
  absolute_expires_at TIMESTAMP NOT NULL,
  last_seen_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  ip                  TEXT NULL,
  ua_hash             TEXT NULL,                     -- sha256(salt+UA)[:8]，原始 UA 永不落库
  device_id           TEXT NULL,                     -- 设备指纹的加盐哈希（见 §5.4）
  device_type         TEXT NULL,                     -- desktop | mobile | tablet | bot | unknown
  device_model        TEXT NULL,                     -- iPhone / Pixel 7 / SM-G991B / Mac …
  device_os           TEXT NULL,                     -- iOS 17.5 / Android 14 / Windows 10/11 …
  device_browser      TEXT NULL,                     -- Safari 17.5 / Chrome 126 …
  revoked_at          TIMESTAMP NULL,
  revoke_reason       TEXT NULL
);
CREATE INDEX ix_sessions_user ON sessions(user_id);
CREATE INDEX ix_sessions_abs  ON sessions(absolute_expires_at);

-- ---------- 邮件发件箱（同时充当限流计数与可观测数据源）----------
CREATE TABLE mail_outbox (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  dedupe_key    TEXT NOT NULL,                       -- 幂等键，见 §7.3
  to_email      TEXT NOT NULL,
  template      TEXT NOT NULL,
  payload       TEXT NULL,                           -- JSON；禁止放明文验证码
  status        TEXT NOT NULL,                       -- pending | sending | sent | failed
  attempts      INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TIMESTAMP NULL,
  last_error    TEXT NULL,
  created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  sent_at       TIMESTAMP NULL
);
CREATE UNIQUE INDEX ux_outbox_dedupe ON mail_outbox(dedupe_key);
CREATE INDEX ix_outbox_pending ON mail_outbox(status, next_attempt_at);

CREATE TABLE mail_log (                              -- 发信请求审计（含被限流拒绝的）
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  email_hash TEXT NOT NULL,
  purpose    TEXT NOT NULL,
  ip         TEXT NULL,
  accepted   INTEGER NOT NULL,                       -- 1 入队 / 0 被拒
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX ix_mail_log_created ON mail_log(created_at);
CREATE INDEX ix_mail_log_email   ON mail_log(email_hash, purpose, created_at);

-- ---------- 管理员审计 ----------
CREATE TABLE admin_audit (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  actor_id    INTEGER NOT NULL,
  actor_email TEXT NOT NULL,
  action      TEXT NOT NULL,
  target      TEXT NULL,
  detail      TEXT NULL,                             -- JSON；禁止放 token / 验证码
  ip          TEXT NULL,
  ua          TEXT NULL,
  created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX ix_audit_created ON admin_audit(created_at);

-- ---------- 迁移版本 ----------
CREATE TABLE schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

### 4.2 必须成立的不变量

| # | 不变量 | 说明 |
|---|---|---|
| I1 | `uid` 一旦非空**永不变更** | 下游可能以 uid 为身份标识；变更只能"停用+重建" |
| I2 | 只有 `active` 用户可以登录 | `invited` / `disabled` 一律拒绝，且响应与"邮箱不存在"完全一致 |
| I3 | 服务端**从不存**明文 token / 明文验证码 / 明文口令 | 只存 `sha256(token)` 与 `hmac(code)` |
| I4 | 同一 `(email, purpose)` 任意时刻**至多一个**未消费验证码 | 由 `ux_otp_active` 保证 |
| I5 | 同一用户任意时刻**至多一个**未消费邀请 | 签发新 token 前必须作废旧 token（同事务） |
| I6 | 所有 token 消费都是**单次**且**原子** | 依赖 `UPDATE … WHERE consumed_at IS NULL` 的受影响行数判定 |
| I7 | 邮件只在事务**提交之后**入队 | 禁止在 SQLite 写事务内做 IO |
| I8 | 任何错误响应都不得包含 SQL 文本、内部路径、token、验证码 | 统一错误文案表见 §6.6 |

### 4.3 索引与清理任务

后台清理（每小时一次，短事务）：

| 目标 | 动作 |
|---|---|
| `otp_codes` | 删除 `expires_at < now - 1h` |
| `invitations` | 删除 `expires_at < now - 30d` |
| `sessions` | 删除 `absolute_expires_at < now` 或 `revoked_at < now - 30d` |
| `mail_outbox` | 删除 `sent_at < now - 30d` |
| `mail_log` / `admin_audit` | 保留 180 天（可配） |

### 4.4 迁移策略

- 用 `internal/migrate` 执行**显式、有序、幂等**的版本化迁移（`schema_migrations` 记录版本号）。
- **不再使用 GORM `AutoMigrate`**（`internal/model/user.go:19` 现行做法）：它无法安全表达 partial unique index、列删除与数据回填。
- 本次为**空库初始化**（清空重建），因此 `0001_init.sql` 即全量 schema；后续变更追加 `0002_*.sql`。

---

## 5. 业务流程

### 5.1 邀请与激活

状态机：

```
（不存在） --admin 建用户--> invited --POST /invite/accept--> active
                                │                              │
                                │ TTL 到期 / admin 撤销          │ admin 停用
                                ▼                              ▼
                             （失效）                       disabled --enable--> active
```

时序：

```
[admin]  POST /admin/users {email}
           ① 归一化 email；校验未被占用
           ② 事务：插入 users(status=invited, uid=NULL)
                   作废该用户既有未消费邀请
                   插入 invitations(token_hash=sha256(tok), expires_at=now+7d)
           ③ 提交后：入队 invite 邮件（dedupe_key=invite:<token_hash>）
           ④ 审计：user_created + invite_sent

[用户]   GET /api/v1/invite?token=…
           ● 只渲染落地页，不做任何写入（含不消费 token）
           ● 页面据"该用户 uid 是否为空"决定是否显示"设置 uid"表单
           ● 响应头：Cache-Control: no-store、Referrer-Policy: no-referrer、X-Robots-Tag: noindex
           ● 前端读到 token 后立刻 history.replaceState 清掉查询串

[用户]   POST /api/v1/invite/check-uid {token, uid}
           ● 校验 token 有效且未消费 → 归一化/校验 uid → 查唯一性
           ● 该 token 累计调用上限 20 次

[用户]   POST /api/v1/invite/accept {token, uid}
           ① 校验 token：存在 / 未消费 / 未过期 / 用户 status=invited
           ② 校验 uid：格式 + 保留字 + 唯一性
           ③ 事务（单一写事务）：
                UPDATE invitations SET consumed_at=now WHERE token_hash=? AND consumed_at IS NULL
                  → 受影响行数 != 1 ⇒ 立即返回 409/410（并发保护）
                UPDATE users SET uid=?, status='active', activated_at=now
                  WHERE id=? AND status='invited' AND uid IS NULL
                  → 受影响行数 != 1 ⇒ 回滚并返回 409
           ④ 提交后：入队 welcome_activated 邮件（dedupe_key=welcome_activated:<user_id>）
           ⑤ 审计：user_activated
           ● 不发放会话（决策 #6）
```

落地页在超时/已消费/不存在时统一提示"链接无效或已过期，请联系管理员重新发送"，**不区分具体原因**。

### 5.2 uid 规则

| 规则 | 取值 | 依据 |
|---|---|---|
| 大小写 | **敏感**（`Alice` ≠ `alice`） | 已确认；SQLite 默认 `BINARY` collation，天然区分大小写 |
| 最短 | 3 字符 | 已确认 |
| 最长 | **32 字符**（建议值，原列 `char(40)` 在 SQLite 不强制） | ⏳ 待确认 |
| 首字符 | 字母 `A-Za-z`（对"不能数字打头"的收紧：同时排除 `_`/`-` 打头） | ⏳ 待确认 |
| 其余字符 | `A-Za-z0-9_-` | ⏳ 待确认 |
| 正则 | `^[A-Za-z][A-Za-z0-9_-]{2,31}$` | 由上推导 |
| 保留字 | **大小写不敏感匹配**（`admin`/`Admin`/`ADMIN` 全部拒绝） | 大小写敏感 + 保留字大小写不敏感，二者缺一即产生仿冒空间 |
| 前后空白 | 拒绝，不做 trim 后接受（避免视觉混淆） | 防仿冒 |
| 不可变性 | 激活后不可修改（I1） | 保护下游身份关联 |

初始保留字表（大小写不敏感）：`admin administrator root system sys api www mail smtp noreply no-reply support help service official security null undefined true false invite invitation otp login logout register signup session token static assets public private user users me self guest anonymous test demo`

收录判据只有一条：**用户取了之后，别人可能误以为是系统或运营方的身份**。

> **不收录宿主/作者专有名词**（产品名、运营账号名、个人 ID）。本库会被多个宿主引用，
> 把某一个宿主或作者的名字写进默认表，等于替别人的部署做决定——这类名字由宿主通过
> `Config.ReservedUIDs` 追加（大小写不敏感，空白项忽略）。
> 已核实的耦合现状：`cloud-core@v1.1.7` 全模块仅 `pkg/export.go:24,30` 两处涉及 uid，且只写入 `ctx.Locals` 而**无任何读取点**，也未参与路径拼接。因此字符集规则的主要目的是**防混淆/防未来误用**，而不是修复既有的路径穿越问题。

### 5.3 登录（邮箱 OTP）

```
[用户]  POST /api/v1/otp/send {email}
          ① 归一化 email；查 users：必须存在且 status=active
          ② 限流检查（先于任何写操作）：email 冷却 / email 日额 / IP 额度 / 全局预算
          ③ 生成 6 位数字码（crypto/rand）
          ④ 事务：作废该 (email,'login') 全部未消费码 → 插入新码(code_hmac, expires_at=now+10m)
          ⑤ 提交后：入队 otp_code 邮件（dedupe_key=otp:<otp_id>）
          ⑥ 无论邮箱是否存在、是否 active，均返回同一响应体与相近耗时

[用户]  POST /api/v1/otp/verify {email, code}
          ① 取该 (email,'login') 未消费码；无 ⇒ 统一错误
          ② 检查 expires_at / attempts < 5
          ③ subtle.ConstantTimeCompare(HMAC(code), code_hmac)
          ④ 失败：UPDATE otp_codes SET attempts = attempts + 1 WHERE id = ? AND attempts < 5
                    （原子递增，且条件更新，杜绝并发绕过）
                  若本次使 attempts 达到 5 ⇒ 同时置 consumed_at（作废）
          ⑤ 成功：UPDATE … SET consumed_at = now WHERE id = ? AND consumed_at IS NULL
                  受影响行数 != 1 ⇒ 已被并发消费，按失败处理
          ⑥ 事务：插入 sessions(idle=now+7d, absolute=now+30d)
          ⑦ 提交后：
                UPDATE users SET last_login_at=now WHERE id=? AND last_login_at IS NULL
                  → 受影响行数 == 1 表示"首次登录" ⇒ 入队 welcome_first_login
                （幂等：并发/重试都不会重复发送欢迎邮件）
```

参数：

| 参数 | 用户登录 | admin 登录 |
|---|---|---|
| 码长 | 6 位数字 | 6 位数字 |
| TTL | 10 分钟 | **5 分钟** |
| 最大尝试 | 5 次 | **3 次** |
| 重发冷却 | 60 秒 | 60 秒 |
| `purpose` | `login` | `admin_login` |

> **`purpose` 必须隔离**：`login` 的码绝不能用于 `admin_login`，否则普通用户可越权换管理员会话。校验查询必须带上 `purpose`。

### 5.4 会话

| 项 | 用户会话 | admin 会话 |
|---|---|---|
| token | 32 字节 `crypto/rand` → base64url | 同 |
| 服务端存储 | `sha256(token)` | 同 |
| 空闲有效期 | 7 天 | 30 分钟 |
| 绝对有效期 | 30 天 | 8 小时 |
| 续期阈值 | `IdleTTL/2`（7 天 → 3.5 天） | `AdminIdleTTL/2`（30 分钟 → 15 分钟）**独立取值** |
| 续期 | 仅当 `idle 剩余 < 阈值` 时推到 `now+idleTTL`，且**不超过** `absolute_expires_at` | 同左 |
| 吊销 | `revoked_at` 置位；logout / 管理员操作 / 停用 / 改邮箱 | 同 |

校验路径（`/session/verify`、`pkg.TokenValid`）：

```
sha256(token) → SELECT … WHERE token_hash=?
  ├─ 未命中 / revoked_at 非空 / now > absolute_expires_at / now > idle_expires_at ⇒ 拒绝
  ├─ 命中：满足续期条件才 UPDATE（避免每请求写库）
  └─ 返回 uid
```

吊销实现（跨进程即时生效，因为两进程共享同一 SQLite）：

```sql
UPDATE sessions SET revoked_at=CURRENT_TIMESTAMP, revoke_reason=?
WHERE user_id=? AND revoked_at IS NULL;
```

> **续期阈值必须按 scope 分别取值。** 管理会话 idle 只有 30 分钟，若沿用用户会话的 3.5 天阈值，
> "剩余不足 3.5 天"永远成立 ⇒ 每次校验都写一次库；反过来若推导顺序写错（先算阈值再补 TTL 默认值），
> 阈值会恒为 0 ⇒ 管理会话永不续期、30 分钟必定掉线。两种情况都已在实现中修正并加了回归测试。

**不做 IP/UA 硬绑定**：移动网络下 IP 频繁变化会误伤；`ip` 与 `ua_hash` 仅用于审计，UA 突变时可作为风险信号记录（本期不阻断）。

**设备指纹（`device_*` 列）**：管理界面要回答"这个用户有几台在线设备、分别是什么"，因此签发会话时把 UA
解析成结构化字段落库（`internal/device`），**原始 UA 仍然不落库**。三条约束：

| 约束 | 原因 |
|---|---|
| `device_id` 是**加盐哈希**，不是指纹原像 | 磁盘上不该出现可跨库比对的设备标识（与 `ua_hash` 用不同的域前缀，两类摘要不通用） |
| 指纹**不含版本号**（`type\|model\|os族\|browser族`） | 否则浏览器/系统升级一次就凭空多出一台设备 |
| 在线设备 = `scope + 设备指纹` 聚合，且必须**排除 idle 已过期**的会话 | 验证码登录每次都签发新会话且不吊销旧的，直接数会话会把"一台手机"显示成"十台设备"；只判 `revoked_at IS NULL` 也不够——会话是按需清理的，库里躺着早就闲置过期的行 |

识别不出来的 UA（空 UA、脚本客户端、Chromium 的 `Android 10; K` 削减方案）**不编造型号**：
`device_id` 记 NULL、型号退化为平台名，与旧数据同归"未知设备"一类。

### 5.5 管理员改邮箱（admin-only）

```
POST /admin/users/:id/email {email}
  ① 归一化 + 唯一性校验
  ② 事务：UPDATE users SET email=? WHERE id=?
            UPDATE sessions SET revoked_at=now, revoke_reason='email_changed' WHERE user_id=?
  ③ 提交后：入队 email_changed_notice 邮件 → 发给【旧地址】
            （内容："你的登录邮箱已被修改，如非本人操作请联系管理员"）
  ④ 审计：email_changed（记录旧值与新值）
```

**已知风险与缓解**：邮箱是唯一凭据，一个手误可能导致用户无法登录。缓解措施：

- admin UI 对邮箱做**二次确认输入**；
- 邮件本身暴露错误（邀请/通知发到错误地址 → 用户永不激活，在"待激活"列表可见）；
- 变更后立即吊销全部会话，避免旧会话被继续使用。

### 5.6 邮件通知清单

| 模板 | 收件人 | 触发时机 | 幂等键 |
|---|---|---|---|
| `invite` | 新用户 | admin 建用户 / 重发 | `invite:<token_hash>` |
| `otp_code` | 用户 / 管理员 | 请求验证码 | `otp:<otp_id>` |
| `welcome_activated` | 新用户 | 激活成功（提交后） | `welcome_activated:<user_id>` |
| `welcome_first_login` | 新用户 | **首次**登录成功 | `welcome_first_login:<user_id>` |
| `email_changed_notice` | **旧地址** | 管理员改邮箱后 | `email_changed:<session?>:<ts>` |
| `admin_login_notice`（建议） | 管理员 | 每次 admin 登录 | `admin_login:<session_id>` |
| `admin_action_notice`（建议） | 管理员 | 建用户/改邮箱/踢会话/删除 | `admin_action:<audit_id>` |

「建议」两封是 admin 只做单因子 OTP 的**补偿控制**：邮箱被盗后的真实利用会被本人立刻发现。

---

## 6. 安全设计

### 6.1 威胁模型与信任边界

| 资产 | 保护目标 |
|---|---|
| 用户账号 | 只有邮箱持有者能登录；管理员可停用/删除 |
| 会话 | 不可伪造、可吊销、服务端不存明文、有绝对上限 |
| admin 面 | 公网不可达；即使管理员邮箱被攻破也无法远程触达 |
| 邮件通道 | 不被当作垃圾邮件炮；域名信誉不被烧毁 |
| 审计数据 | 事件可溯源（谁、何时、对谁、从哪来） |

信任边界：

1. **Caddy 是唯一公网入口**，`public :9999` 必须通过防火墙限制为"只允许 Caddy 访问"（否则 TLS、限流、日志全部可被绕过）。
2. **admin 端口只绑回环**，判定仅用 `RemoteAddr`。
3. **SQLite 文件是信任边界内的强资产**：能读它的人可读取全部邮箱、审计与（哈希后的）会话，因此文件权限必须收敛。

### 6.2 限流矩阵

| 作用域 | 限制 | 存储 | 超限响应 |
|---|---|---|---|
| `otp/send` per (email,purpose) | 60s 冷却 + 5/小时 + 10/天 | SQLite（`mail_log`） | 429 + `Retry-After` |
| `otp/send` per IP | 10/小时 + 30/天 | SQLite（内存粗筛） | 429 |
| `otp/verify` per (email,purpose) | 5 次/码（admin 3 次） | SQLite（`attempts`） | 401（统一文案） |
| `otp/verify` per IP | 30/小时 | 内存 | 429 |
| `invite/accept` per IP | 10/小时 | 内存 | 429 |
| `invite/check-uid` per token | 20 次 | SQLite（`invitations.sent_count` 同源计数） | 429 |
| **全局熔断** | 日发信量 100 封（可配） | SQLite | 停止非 OTP 发送 + 告警 |
| admin 发邀请 | 20/小时 | SQLite | 429 |
| admin API 全局 | 60/分钟 | 内存 | 429 |
| `session/verify` | 仅 Caddy 粗筛 + 单 IP 300/分钟兜底 | 内存 | 429 |

**三条硬要求**：

1. **必须是 429 状态码**，而不是 body 里写个 `code`。这要求先修复 `internal/helper/response/filber.go:19-24` 不设置状态码的问题。
2. **安全关键计数器落 SQLite**（进程内计数器会在重启后清零，而现有代码存在 panic 重启路径）。
3. **退避而非永久锁定**：否则攻击者可以故意打满目标的限流，把真实用户锁在门外。

### 6.3 邀请 token 安全

| 风险 | 处理 |
|---|---|
| **邮件安全网关预取链接**（Defender SafeLinks / Proofpoint / Mimecast / 链接预览） | **GET 绝不消费 token**，只有 `POST /invite/accept` 消费。否则用户点开永远看到"链接已失效" |
| token 进入服务端日志 | Caddy 默认 JSON 访问日志记录 `request.uri`（含查询串）。Caddy 的 `log` 指令无法只脱敏部分 URI ⇒ 二选一：(a) 该站点**关闭访问日志**（推荐，本服务流量小）；(b) 接受该暴露（单次 + 7 天 TTL 已收敛窗口）。**应用自身日志永远不得记录 token** |
| token 进入浏览器历史 / Referer | 页面加载后 `history.replaceState` 清除查询串；`Referrer-Policy: no-referrer`（现代浏览器跨源默认只发 origin，主要暴露面是历史与日志） |
| 被搜索引擎收录 | `X-Robots-Tag: noindex` + `Cache-Control: no-store` |
| 转发 / 猜解 | 32 字节 `crypto/rand`、只存哈希、TTL 7 天、单次消费 |
| 多 token 并存 | 重发/复制 = 事务内先作废该用户全部未消费邀请，再插入新 token（I5） |
| 并发抢同一个 uid | 由 `ux_users_uid` 唯一索引裁决，冲突返回 **409 + 友好文案**，绝不透出 SQL 文本 |
| 备用方案（备选） | token 放 URL fragment（不发给服务端）可彻底避开日志，但部分邮件网关的链接改写会破坏 fragment，**本期不采用** |

### 6.4 真实 IP 与代理信任（最容易踩的坑）

**已验证的 Fiber v2 行为**：

- `Ctx.IP()`：仅当 `IsProxyTrusted() && ProxyHeader != ""` 时读头，否则用 `RemoteIP()`。
- `IsProxyTrusted()`：**`EnableTrustedProxyCheck` 默认 false 时无条件返回 true**。
- `extractIPFromHeader()`：未开启 `EnableIPValidation` 时"**直接返回头里原样内容**"。

⇒ 只要为了限流配上 `ProxyHeader` 而没开 `EnableTrustedProxyCheck`，**任何客户端自带 `X-Forwarded-For: 127.0.0.1` 就能同时绕过按 IP 限流和"仅本地访问"判定**。

**public 进程正确配置**：

```go
fiber.New(fiber.Config{
    EnableTrustedProxyCheck: true,
    TrustedProxies:          []string{"127.0.0.1"},   // Caddy 所在主机地址
    ProxyHeader:             "X-Real-IP",
    EnableIPValidation:      true,
})
```

**Caddy 侧必须覆盖式写入**（不是追加，追加时最左段是客户端可控的）：

```caddyfile
id.example.com {
    encode zstd gzip
    header {
        Strict-Transport-Security "max-age=31536000; includeSubDomains"
        X-Content-Type-Options    nosniff
        Referrer-Policy           no-referrer
        -Server
    }
    @admin path /admin/*
    respond @admin 404

    reverse_proxy 127.0.0.1:9999 {
        header_up X-Real-IP {http.request.remote.host}
        header_up X-Forwarded-For {http.request.remote.host}
    }
}
```

**admin 进程**：不设 `ProxyHeader`、不开 `EnableTrustedProxyCheck`，本地判定只认 `RemoteAddr ∈ {127.0.0.1, ::1}`。

**补充**：Caddy 是标准二进制，**不含限流插件**（`caddy-ratelimit` 需自定义构建）。因此限流以应用层为主，边缘层用 **Caddy JSON 访问日志 + fail2ban** 实现 IP 封禁（这正是最初诉求的"IP 封禁"）。

### 6.5 admin 面本机防护

**"本地"不等于"可信"**：同主机任意本地进程、任何 SSRF、以及管理员浏览器里的恶意页面都能访问 `127.0.0.1:9998`。因此：

| 措施 | 说明 |
|---|---|
| 只绑 `127.0.0.1:9998` | 且 Caddy 明确拒绝 `/admin/*` |
| **API 自身必须鉴权** | 不能依赖"页面只有管理员看得见"；所有 `/admin/*` 都要求 admin 会话 |
| `Host` 校验 | 仅接受 `127.0.0.1:9998` / `localhost:9998` ⇒ 挡 DNS rebinding |
| `Origin` 精确匹配 | 跨源请求一律拒绝 |
| **不用 cookie，改用内存 token + `Authorization` 头** | 从根本上消除 CSRF（无环境凭据）；也回避 localhost 上 `Secure` cookie 的限制。代价：刷新页面需重新登录 |
| 严格 CSP | `default-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'` ⇒ 要求 JS 放独立文件、**禁用内联脚本** |
| 静态资源 | 固定 root，禁目录列表，注意路径穿越 |
| 审计 | 全部写操作落 `admin_audit` |

### 6.6 日志与错误响应禁忌

| 禁忌 | 原因 |
|---|---|
| 日志/响应中出现验证码、邀请 token、会话 token | 直接等于凭据泄漏 |
| 响应中出现 `err.Error()`（含 SQL 文本、约束名、文件路径） | 现行 `internal/api/manager/manager.go:37,76,108` 的教训 |
| 登录类接口区分"邮箱不存在"与"账号未激活/已停用" | 用户枚举 |
| 认证失败返回 HTTP 200 | 网关/WAF/监控/客户端全部看不见，这正是现状 P1-4 |

对外统一文案表（草案）：

| 场景 | HTTP | 文案 |
|---|---|---|
| 发码成功（含邮箱不存在） | 200 | `如果该邮箱已注册，验证码已发送` |
| 验证码错误/过期/超次 | 401 | `验证码无效或已过期` |
| 邀请链接无效/过期/已使用 | 410 | `链接无效或已过期，请联系管理员重新发送` |
| uid 已被占用 | 409 | `该用户名已被使用` |
| uid 不合规 | 400 | 具体规则提示（这是用户体验必要的唯一例外） |
| 限流 | 429 | `请求过于频繁，请稍后再试` + `Retry-After` |
| 服务端异常 | 500 | `服务暂时不可用` |

### 6.7 凭据与配置管理

| 项 | 要求 |
|---|---|
| SMTP 凭据（三方客户端安全密码） | **环境变量**，禁止硬编码/入库（吸取 `loveVG` 教训） |
| 验证码 HMAC 密钥 | 环境变量；独立于会话密钥 |
| 会话/邀请 token | 随机生成，无需密钥；服务端只存哈希 |
| 发件人地址、站点基址 | 环境变量（邀请链接需绝对 URL） |
| 启动自检 | 必需环境变量缺失时**启动失败并明确报错**，而不是用默认弱值兜底 |

必需环境变量（草案）：`UC_DB_PATH`、`UC_PUBLIC_BASE_URL`、`UC_SMTP_HOST`、`UC_SMTP_PORT`、`UC_SMTP_USER`、`UC_SMTP_PASS`、`UC_SMTP_FROM`、`UC_HMAC_KEY`、`UC_MAIL_DAILY_QUOTA`（实测的日发信上限；§7.2 的熔断阈值取它与 100 的较小值）。

---

## 7. 邮件子系统

### 7.1 阿里企业邮箱 SMTP 配置

已确认使用**阿里企业邮箱（免费版）**，且**自有域名已绑定** ⇒ SPF/DKIM/DMARC 可自行配置，到达率与发件信誉可控（这是相对 `@163.com` 这类个人邮箱的关键改善：个人邮箱无法为自己的域名做对齐）。

**服务器参数**（来源：阿里云官方帮助中心）

| 项 | 值 |
|---|---|
| SMTP 服务器 | `smtp.qiye.aliyun.com`（旧地址 `smtp.mxhichina.com` 仍有效） |
| 端口 | **`465`（SSL，推荐）**；`25` 为非加密端口。**80 / 587 未开通**，不要尝试 |
| TLS 证书 | 官方服务器地址已预置受信任证书，直连 465 即可。**不要**用自定义域名当 SMTP 地址（那样需自行购买并上传证书） |
| 认证用户名 | 完整邮箱地址 |
| 认证口令 | **「三方客户端安全密码」**（见下方前置条件），不是邮箱登录密码 |
| 发件人 | 必须是**真实存在、能收信**的邮箱（退信要能收回来做监控），建议 `noreply@<你的域名>` 且该地址真实可用 |
| 实现 | `net/smtp`（TLS 直连 465）+ 自建 outbox worker；或轻量库（如 `gomail`）。**连接必须 TLS** |

**⚠ 两个会直接卡住联调的前置条件**（官方文档明确要求，先在管理后台配好再写代码）：

1. **阿里邮箱默认禁止三方客户端登录**：需邮箱管理员为该账号开启「第三方客户端登录权限」。
2. 需管理员开启该账号的 **POP3/IMAP 服务权限**，并开启/设置「**三方客户端安全密码**」作为 SMTP 登录口令。
   若组织强制开启了该功能，**填邮箱登录密码会直接报"用户名或密码错误"**——这是最常见的踩坑点。

**DNS 配置清单**（发信到达率的决定因素；MX 应已随域名绑定完成，重点补 SPF/DKIM/DMARC）：

| 记录 | 主机 | 值 | 来源与可靠性 |
|---|---|---|---|
| MX | `@` | `mx1.mail.aliyun.com`（优先级 5）、`mx2.mail.aliyun.com`（优先级 10） | 社区口径；应与邮箱后台一致 |
| **SPF** | `@` | `v=spf1 include:spf.qiye.aliyun.com -all` | 社区口径，**务必以邮箱后台「域名管理」给出的值为准** |
| **DKIM** | 后台生成的 selector 主机名 | 后台开启 DKIM 后给出的公钥值 | **必须从后台取**，无法自行推导 |
| **DMARC** | `_dmarc` | `v=DMARC1; p=none; rua=mailto:dmarc@<你的域名>` | 通用做法 |

DMARC 演进建议：先 `p=none` 观察 2 周聚合报告，确认 SPF/DKIM 均对齐后再逐级升到 `quarantine` → `reject`。**缺少 DMARC 时，邀请与验证码邮件进垃圾箱的概率显著上升**，而这会直接表现为"用户收不到验证码，无法登录"。

**配额**：社区口径为**每账号每日 200 封**（另有"阿里云主账户总量额度"的说法，且需注意区分**企业邮箱**与**邮件推送 Direct Mail** 两个产品的额度——后者按信誉等级从 2000 封/日起）。官方文档未给出免费版的明确数值 ⇒ ⏳ **上线前必须实测**，并把实测值填入 `UC_MAIL_DAILY_QUOTA`（§7.2 的熔断阈值由它推导）。

### 7.2 配额保护（小规模部署的简化版）

**容量不是约束**：本系统用户量为个位数，正常消耗是"每用户每天几封"。真正的风险只有一个——**攻击者把当日配额刷光，导致所有人拿不到新会话**（已有会话不受影响，见 §6.1）。

这个风险有上界，而且很好算：

> **最大可被烧掉的量 = 单邮箱日上限 × 已注册用户数**

因为**未注册邮箱不发信**（§5.3 的"空发送"路径只返回统一响应、不消耗任何配额）。

| 用户数 | 单邮箱日上限 | 最坏日消耗 | 对照配额 200 封/日 |
|---|---|---|---|
| 5 | 10 封 | ≤ 50 封 | 烧不光 |
| 10 | 10 封 | ≤ 100 封 | 仍在安全线内 |

⇒ 个位数用户规模下，**只要收紧"单邮箱日上限"这一个旋钮，配额就不可能被刷光**。

> **⚠ 多宿主共享发件账号的情况**：若 N 个宿主共用同一个发件邮箱（例如都用 `noreply@你的域名`），则**服务商配额与发信信誉是共享的**，上界要按"所有宿主当日消耗之和"计算。建议每个宿主使用**独立的发件地址**（`svc-a@`、`svc-b@`）：既隔离配额与信誉，也让"这封信来自哪个服务"一目了然。同时，邀请邮件里出现的服务名与落地页域名应与 `Config.PublicBaseURL`（宿主自己的域名）一致，让用户看到的是他熟悉的那个服务。

因此本节只保留下面 6 条，其余策略一律不做：

| 措施 | 规则 | 作用 |
|---|---|---|
| **单邮箱冷却** | 60 秒（按"最近一次成功入队"计算） | 防连点与骚扰 |
| **单邮箱日上限** | 10 封/天（可配） | **界定上界的核心旋钮** |
| **冷却期复用** | 冷却期内重复 `/otp/send` **不发信、不生成新码**（旧码继续有效），响应保持一致 | 省配额 + 防有人用你的域名骚扰用户 |
| **统一响应** | 未注册 / 未激活邮箱走同一路径，响应体与耗时一致 | 防枚举 |
| **全局熔断** | 日发信量达 **100 封**（可配）时停止非 OTP 发送并告警 | 廉价保险，避免 provider 侧封停 |
| **统计窗口** | 滚动 24 小时 | 避免零点重置被集中滥用 |

**本期不做**（用户量增长或更换 provider 时再评估；实现时给 `mail_outbox` 留一个 `priority` 列即可，零成本）：

- 邀请 / 通知 / OTP 的优先级队列
- 邀请类邮件占日配额的百分比预留
- 用量超阈值后的自适应节流（拉长冷却、暂停邀请）
- 60% / 80% / 90% 多级告警

**唯一需要留意的时间点**：把邀请一次性发给全部用户（例如首次上线）会瞬间消耗"用户数 × 3"封邮件，这在个位数规模下也只是十几封，无需特殊处理。

### 7.3 队列与投递（SQLite 即队列）

单实例前提下，**不需要内存队列**：`mail_outbox` 表本身就是持久化队列，进程重启不丢信。

```
入队：INSERT INTO mail_outbox(dedupe_key, to_email, template, payload, status='pending')
      -- dedupe_key 唯一索引 ⇒ 重复入队直接失败（幂等）
worker：SELECT … WHERE status='pending' AND next_attempt_at <= now ORDER BY id LIMIT 1
        UPDATE … SET status='sending', attempts=attempts+1   -- 抢占
        SMTP 投递
        成功 → status='sent', sent_at=now
        失败 → status='pending', next_attempt_at = now + backoff(attempts)
               attempts >= 5 ⇒ status='failed' + 告警
```

退避策略：1m → 5m → 15m → 1h → 6h（共 5 次）。

**注意**：SMTP 投递成功只代表"已交给服务器"，不代表送达。Bounce 只能通过阿里邮箱侧的退信邮件人工/定期查看 ⇒ **发件人必须是真实可收信的邮箱**，且上线后头两周要人工核对退信。

### 7.4 幂等键规则

| 模板 | dedupe_key | 保证 |
|---|---|---|
| `otp_code` | `otp:<otp_id>` | 同一验证码只发一封 |
| `invite` | `invite:<token_hash>` | 同一邀请链接只发一封（重发=新 token=新键） |
| `welcome_activated` | `welcome_activated:<user_id>` | 无论重试/并发都只发一封 |
| `welcome_first_login` | `welcome_first_login:<user_id>` | 同上，配合 `last_login_at IS NULL` 条件更新 |
| `email_changed_notice` | `email_changed:<user_id>:<unix_ts>` | 每次变更一封 |

### 7.5 模板规范

- 纯文本为主 + 极简 HTML（**不放外部图片/追踪像素/第三方字体**）。
- 正文含：一句话说明 + 链接/验证码 + 有效期 + "如非本人操作请忽略"。
- **验证码不写进邮件主题**（主题会被预览、进通知栏）。
- 正文**不含任何额外个人信息**（不列 uid、不列其他账号信息）。
- 发件人显示名固定（如 `user-center`），避免被判定为仿冒。
- 事务邮件**不需要** `List-Unsubscribe`。

---

## 8. admin 子系统

### 8.1 功能范围

新增用户（发邀请）、重发邀请、复制邀请链接、停用/启用、删除、改邮箱、查看与踢出会话、查看审计。**不提供**：自助注册、用户自助改邮箱、用户自助改密码（已无口令概念）。

### 8.2 bootstrap（配置驱动，无公开注册入口）

每个宿主服务都有自己的管理员，因此 bootstrap 是**宿主的配置**，而不是一条全局命令：

```go
uc.New(uc.Config{
    DBPath:              "/var/lib/svc-a",              // 该宿主自己的库
    PublicBaseURL:       "https://svc-a.example.com",   // 邀请链接用该宿主的域名
    BootstrapAdminEmail: "ops@example.com",             // 仅当库内尚无管理员时生效
    Mailer:              smtpMailer,                    // 由宿主注入
})
```

- 语义：启动时若**库内不存在任何管理员**，则把该邮箱创建为管理员（状态直接为 `active`，避免唯一管理员在点开邮件前无法登录）；**已存在管理员时忽略**（幂等）。
- 管理员同样需要一个用户名，因此引导时会发一封「设置用户名」的一次性链接邮件；
  若该管理员已有一条未过期的链接则不重发（既不刷屏，也不会作废对方邮箱里那条）。
  实现见 `internal/app.Bootstrap` 与 `BootstrapResult`；落地页文案由 `InviteView.AlreadyActive` 区分。
- 该值只存在于宿主的配置/环境变量中；库**不暴露任何"注册管理员"的 HTTP 接口**。
- N 个宿主各自配置自己的管理员 ⇒ 天然的按服务隔离，无中心权限。

### 8.3 静态 UI 规范

| 要求 | 说明 |
|---|---|
| 零外部依赖 | 不用 CDN、不用第三方字体/统计脚本 ⇒ 无供应链风险，且可上严格 CSP |
| 无内联脚本/样式 | JS/CSS 独立文件 ⇒ 无需 nonce 即可启用 `script-src 'self'` |
| 无 cookie | 会话 token 只存 JS 内存变量，随 `Authorization` 头发送 ⇒ 无 CSRF 面 |
| 不持久化敏感数据 | 不用 localStorage；刷新即需重新登录 |
| 渲染安全 | 一律 `textContent`，禁止把接口数据拼进 `innerHTML` |
| 请求头 | 所有写请求带 `Origin` 校验所需的同源信息 + 自定义头（触发预检） |
| 文件 | `internal/adminui/web/{index.html, app.js, style.css}`（经 `embed.FS` 打进库）+ 落地页 `invite.html`（归公共面） |

### 8.4 "复制邀请链接"的行为约束

由于服务端**只存 `sha256(token)`**，明文 token 无法二次取回，因此：

- **语义 = 重新生成**：点击按钮 ⇒ 事务内作废旧 token、签发新 token、返回明文一次；UI 必须提示"**旧链接将立即失效**"。
- 响应头 `Cache-Control: no-store`；明文只出现在本次响应体，不落库、不落日志。
- 写审计：`invite_link_regenerated`（记录操作者、目标用户、时间、IP）。
- 与"重发邀请邮件"共用同一实现（重发 = 重新生成 + 入队邮件）。
- **失败模式**：若用户此前已收到邮件但未点击，复制新链接会让邮件里的链接失效。管理员操作前需知悉（UI 文案提示）。

### 8.5 审计事件表

`user_created`、`invite_sent`、`invite_resent`、`invite_link_regenerated`、`user_activated`（系统）、`user_disabled`、`user_enabled`、`email_changed`、`sessions_revoked`、`user_deleted`、`admin_login`、`admin_login_failed`、`admin_logout`。

每条记录：`actor_id / actor_email / action / target / detail(JSON) / ip / ua / created_at`。**`detail` 禁止写入 token、验证码或邮箱正文**。

---

## 9. 上线与回滚 runbook

> **库形态下，本流程要按宿主逐个执行。** 每个宿主有自己的 DB、用户与管理员，升级节奏也各自独立（§0.1 L6/L9）。宿主之间没有共享状态，因此可以灰度：先升一个宿主观察，稳定后再推其余。

### 9.1 前置准备

1. Caddy：覆盖式 `X-Real-IP`、`/admin/*` 返回 404、HSTS 等安全头、按需关闭该站点访问日志（§6.3）。
2. 防火墙：`9999` 仅允许 Caddy 来源；`9998` 不对外开放。
3. 环境变量就位（§6.7），并确认缺失时进程**拒绝启动**。
4. 阿里企业邮箱：管理员开启该账号的**三方客户端登录权限**与 **POP3/IMAP 权限**、生成**三方客户端安全密码**；配好 **SPF / DKIM / DMARC**；**实测日发信上限**（详见 §7.1）。
5. **归档（仅供紧急回滚，不导出 uid 清单）**：旧 `user-center.db`、`pebble-token-auth.db` 目录。

### 9.2 每个宿主服务的升级顺序

```text
对每一个宿主（例如先 cloud-core）：

1) 升级依赖 tag：go get github.com/tangthinker/user-center@v2.x.y
   （TokenValid / RegisterUserCenter 垫片保留 ⇒ 宿主业务代码无需改动）
2) 停该宿主进程
3) 归档该宿主的旧 user-center.db 与 pebble-token-auth.db 目录（回滚要用，见 9.4）
4) 该宿主首次启动：库自动执行 schema 迁移；随后检查是否出现"schema 版本高于本库支持"的报错
5) 在宿主配置里设置 BootstrapAdminEmail（仅首个管理员生效）
6) 邮件链路自测：给自己建用户 → 收到邀请 → 设 uid → 激活 → 收到 welcome_activated
                 → OTP 登录 → 收到 welcome_first_login
7) 确认 admin 面只挂在回环监听上：从另一台机器访问 :9998 应当连接失败
8) admin 逐个创建该宿主的用户并发邀请
9) 观察 24h：发信成功率、OTP 失败率、限流触发次数、429 比例、admin 审计流水
10) 稳定后再升级下一个宿主
```

**注意**：第 3 步的归档不是可选项——旧版本的库会拒绝在新的 schema 上启动（§12.2），因此回滚必须连带恢复 DB 备份。

### 9.3 观测指标（最小集）

| 指标 | 告警阈值（建议） |
|---|---|
| 发信失败率 | > 10% 持续 10 分钟 |
| 全局发信量 | 达到熔断阈值 80% 时告警（§7.2） |
| `otp/verify` 失败率 | > 50% 持续 10 分钟（可能是爆破） |
| 429 比例 | > 5% 持续 10 分钟 |
| `mail_outbox` 积压 | pending > 50 或最老 pending > 30 分钟 |
| 待激活用户数 | 邀请发出 3 天后仍未激活（人工跟进，非告警） |

### 9.4 回滚

- **回滚点**：旧二进制 + `user-center.db`/`pebble` 归档。
- **代价**：回滚会丢失所有新建用户、邀请与已激活账号；旧库恢复后等于回到改造前状态（含 MD5 口令面）。
- 回滚触发条件（建议）：邮件链路整体不可用且 24h 内无法修复，或激活流程存在阻塞性缺陷。
- **break-glass**（邮件故障但服务需继续）：主机上执行 `admin session mint --email=<管理员邮箱>` 直接签发一个短时 admin 会话；该命令写审计并打印会话 token。这是"邮件挂了也能进入后台"的兜底路径。

---

## 10. 验收测试清单

**邀请与激活**

1. 邮件网关/链接预览预取 `GET /invite?token=…` 后，用户仍能正常激活（GET 不消费）。
2. 同一 token 第二次 `POST /invite/accept` 失败（410）。
3. 重发邀请后，旧链接立即失效。
4. 邀请过期后提交，返回统一文案且不透露具体原因。
5. 并发两个请求抢同一 uid：一个成功、一个 409，且响应中**无 SQL 文本**。
6. `check-uid` 无 token 或 token 无效时被拒；同一 token 调用超过 20 次被 429。
7. 保留字大小写不敏感：`admin` / `Admin` / `ADMIN` 全部被拒。
8. uid 规则边界：2 字符拒绝、数字打头拒绝、3 字符通过、32 字符通过、33 字符拒绝。
9. 大小写敏感生效：`Alice` 与 `alice` 可共存；但 `Admin` 仍被保留字拒绝。
10. 激活成功后 **不发放会话**；用激活时的响应无法换取任何 token。

**登录与会话**

11. 连发 20 个验证码后，只有最后一个有效。
12. 同一验证码错 5 次即作废；第 6 次不消耗任何计数。
13. 并发 10 个 `otp/verify` 无法突破 5 次上限。
14. 未注册邮箱 / 未激活 / 已停用邮箱的响应体与耗时与正常情况一致。
15. `login` 的验证码不能用于 `admin_login`。
16. 会话续期不突破 `absolute_expires_at`。
17. `logout` 后旧 token 立即失效；管理员踢会话后旧 token 立即失效。
18. admin 改邮箱后，该用户全部会话立即失效，且旧地址收到通知。
19. 首次登录欢迎邮件在并发/重试下只发一封。
20. 重启进程后限流计数与会话状态仍然有效（落库）。

**admin 面与边界**

21. 伪造 `X-Forwarded-For: 127.0.0.1` / `X-Real-IP: 127.0.0.1` 既不能访问 admin，也不能绕过限流。
22. `Host: evil.com` 访问 admin 被拒（DNS rebinding）。
23. 从外部站点向 `127.0.0.1:9998/admin/*` 发起跨站 POST 被拒。
24. 普通用户会话无法调用任何 `/admin/*` 接口。
25. admin 每次登录与每次写操作都有审计记录与（可选的）邮件通知。
26. 非管理员邮箱请求 `/admin/otp/send` 被拒，且响应不暴露该邮箱是否为管理员。

**限流、日志与运维**

27. 超出限流返回 **429 + `Retry-After`**（不是 HTTP 200 里写 code）。
28. 限流是退避而非永久锁定：等待窗口后自动恢复。
29. 任何日志、错误响应、审计 `detail` 中都不出现验证码或 token。
30. 缺必需环境变量时进程启动失败并给出明确错误。
31. `mail_outbox` 积压或失败时产生告警；投递失败按退避重试且不重复发送。
32. cloud-core 侧：`pkg.TokenValid` 与 `ctx.Locals("uid")` 行为不变（回归验证）。

---

## 11. 未决问题

| # | 问题 | 建议默认值 | 影响 |
|---|---|---|---|
| Q1 | uid 首字符是否允许 `_` / `-`；最长 32 还是 40 | 仅字母打头、最长 32 | 落地页表单校验与保留字表 |
| Q2 | 邀请落地页 URL 归属：挂在内嵌库的路由组（`/api/v1/invite`）还是由 cloud-core 单独挂 `/invite` | 挂在库的路由组内，改动最小 | 邮件里的链接形态 |
| Q3 | 阿里企业邮箱免费版的**实测**日发信上限（社区口径 200 封/日，官方未明确） | 上线前实测 | §7.2 熔断阈值与容量判断 |
| Q4 | 是否需要 `admin_login_notice` / `admin_action_notice` 两封管理员通知 | 建议要（OTP-only + 仅本地的补偿控制） | 邮件量与实现量 |
| Q5 | 用户自助改邮箱是否需要（当前决策为 admin-only） | 保持 admin-only | 若开放，需三步验证 + 通知旧地址 + 吊销会话 |
| Q6 | 升级登录方式时是否需要给存量用户发一封公告邮件 | 用户量小可不发 | 邮件量 |
| Q7 | 会话参数（idle 7 天 / absolute 30 天）是否接受 | 接受 | 安全与体验平衡 |
| **Q8** | 是否升主版本 **v2**（库形态下做破坏性 API 变更的唯一合法方式） | 升 v2，并保留 v1 垫片 | 决定 cloud-core 是否需要改代码 |
| **Q9** | 是否保留 `RegisterUserCenter` / `TokenValid` 垫片（决策 #19） | 保留（默认实例 + 惰性初始化） | 决定 cloud-core 的升级成本 |
| **Q10** | N 个宿主是否共用同一个发件邮箱 | 建议每个宿主独立发件地址 | 配额与发信信誉是否隔离（§7.2） |
| **Q11** | 库是否内置 SMTP `Mailer` 默认实现 | 内置（配置全部来自 `Config`，不读 env） | 宿主可零成本接入，也可自行替换 |
| **Q12** | admin 静态 UI 由库 `embed` 还是宿主自备 | 库 embed 一套默认 UI，宿主可覆盖 | 决定各宿主 admin 是否长得一样 |
| **Q13** | 跨宿主的身份是否要打通（同一人在多个服务用同一身份） | 本期不做，各自独立 | 一旦要做就需要中心化身份，形态会变 |

---

## 12. 版本化与兼容策略（库形态的硬要求）

已发布的 tag 是**不可修改的契约**（Go module 语义），而每个宿主按自己的节奏升级 ⇒ 兼容性必须由设计保证，不能靠"大家一起升级"。

### 12.1 API 兼容规则

| 变更类型 | 规则 |
|---|---|
| 新增导出函数/字段 | 允许，次版本号（`v2.x`） |
| 修改导出签名、删除导出项、改变语义 | **必须升主版本**（`v3`） |
| 兼容垫片 | v2 保留 `RegisterUserCenter(router, path)` 与 `TokenValid(token) (string, error)`，内部包装"默认实例" ⇒ **cloud-core 不改代码即可从 v1 升到 v2** |

### 12.2 Schema 兼容（跨 tag 共存的现实）

现状证据：`cloud-core@v1.1.6` 与 `v1.1.7` 都锁在 `user-center v1.0.7`，而工作区已领先 ⇒ **同一时间必然有不同版本的宿主在运行**。

| 规则 | 说明 |
|---|---|
| **同主版本内只做加法** | 新列必须可空或有默认值；新表独立；不重命名、不改类型、不删列 |
| **破坏性 schema 变更 = 主版本** | 且必须提供迁移脚本与回退说明 |
| **旧库遇新 schema 拒绝启动** | 启动时读 `schema_migrations` 最大值；若高于本库已知最高版本 ⇒ **返回 error，绝不继续读写**（防止旧版本库悄悄破坏新结构） |
| **迁移自动执行** | 在 `New()` 内执行，带进程内互斥；提供 `Config.SkipMigration` 供只读/演练场景 |
| **回滚必须连带恢复 DB 备份** | 因为旧版本库会拒绝在新 schema 上启动（§9.2 第 3 步、§9.4） |

### 12.3 每个 tag 的发布前清单

1. 是否修改了导出 API？→ 是则升主版本。
2. 是否有 schema 变更？→ 只用加法；补幂等且可重入的迁移函数。
3. 是否引入了新的 goroutine / 文件句柄 / 定时器？→ 必须由 `Close()` 回收（§0.1 L8）。
4. 是否新增了对全局 env 的读取？→ 不允许，改走 `Config`。
5. 是否新增了可能 panic 的路径？→ 不允许，改为返回 error。
6. 是否写死过任何密钥/域名/端口？→ 不允许（`internal/constrant/auth.go:7` 的 `TokenSecret = "Tangthinker"` 就是反例）。
7. 在 `t.TempDir()` 上跑通完整流程：创建实例 → 建用户 → 邀请 → 激活 → 登录 → 吊销 → `Close()`；并额外验证**两个实例互不干扰**。

---

## 附录 A：与现状代码的差异清单

### A.1 删除

| 路径 | 依据 |
|---|---|
| `internal/helper/pwdencry/`（含测试） | 唯一引用是 `internal/service/manager/manager.go:8,24,31` |
| `internal/helper/idgen/`（含测试） | 已是死代码（仅其自身测试引用）；且用 `math/rand` + MD5，不适合安全用途 |
| `internal/service/auth/jwt.go` | 随之删除 `constrant.TokenSecret`，卸掉硬编码 HS256 密钥 |
| `internal/service/auth/memory.go`（含测试） | `sync.Map` 只增不减 + 错误信息回显 token |
| `internal/service/auth/pebble.go` | Pebble 退役；亦是"消费者 panic 后全站阻塞"的来源 |
| `internal/service/auth/common.go` | `genToken` / `TokenInfo` 由新会话模块取代 |
| `internal/constrant/auth.go` | 引用方仅上述三个实现 |
| `req/req.http` 中的 `loveVG`、示例口令与历史 JWT | 仓库内凭据清理 |

### A.2 重写 / 改造

| 路径 | 改动 |
|---|---|
| `internal/schema/user.go` | 去掉 `Password` 与 `json:"password"`；新增 email/status/is_admin 等 |
| `internal/model/user.go` | 扩展查询；移除 `AutoMigrate`，改用显式迁移 |
| `internal/db/sqlite.go` | DSN 增加 WAL 等参数；初始化失败返回 error 而非 panic |
| `internal/service/manager/manager.go`、`internal/api/manager/manager.go`、`internal/data/login.go` | 由登录/注册/改密改写为 OTP + 邀请 + 会话 |
| `internal/helper/response/filber.go` | `Error()` 必须设置 HTTP 状态码；新增统一错误文案 |
| `pkg/fiber_adapter.go` | 路由表重写；**`TokenValid(token) (string, error)` 签名与语义保持不变** |
| `main.go` | 增加 Fiber 代理信任配置、限流中间件、优雅关闭 |
| `go.mod` | 移除 pebble（连带大量 indirect）；新增邮件与限流相关依赖 |

### A.3 新增

`usercenter.go`（`Config`/`New`/`Close`）、`legacy.go`（v1 垫片）、`internal/{store,mail,ratelimit,otp,invite,session,audit,adminui,migrate}/`。

### A.5 库形态带来的结构性改动（v2.0 新增）

| 项 | 现状 | 目标 |
|---|---|---|
| 实例化 | 全局单例 + `sync.Once`（`internal/db/sqlite.go:10-32`、`internal/service/auth/pebble.go:22-25`） | `New(Config)` 返回实例句柄，状态挂在实例上 |
| 初始化失败 | `panic`（`internal/db/sqlite.go:25`、`internal/service/auth/pebble.go:36`） | 返回 `error`，绝不 panic |
| DB 路径 | 全局 `SetDBPath`，首次初始化后失效 | `Config.DBPath`，每实例独立；移除全局 setter |
| 迁移时机 | 构造 `UserModel` 时 `AutoMigrate`（`internal/model/user.go:19`） | `New()` 内执行显式版本化迁移，支持 `SkipMigration` |
| admin 面 | 无 | `AdminHandler()` 由宿主挂到回环监听 + `LocalOnly`（只认 `RemoteAddr`） |
| 配置来源 | 库内部硬编码（`internal/constrant/auth.go:7`）与全局 env | `Config` 注入，宿主决定来源 |
| 生命周期 | 无 `Close()`；`pebble.go:43-52` 的消费者 goroutine 无法回收 | `Close()` 回收 goroutine、文件句柄与定时器 |
| 兼容垫片 | — | `RegisterUserCenter` / `TokenValid` 包装默认实例，保证 cloud-core 零改动升级 |

### A.4 下游影响

- `cloud-core@v1.1.7` 依赖 `user-center v1.0.7`，使用 `pkg.RegisterUserCenter` 与 `pkg.TokenValid`（`cloud-core@v1.1.7/main.go:67`、`pkg/export.go:24`）。
- 本次删除 `/login`、`/register`、`/modify-password`、`/uid-unique` 属**破坏性变更** ⇒ 需升主版本（v2）或提供兼容壳。
- 只要 `TokenValid` 签名不变，`ctx.Locals("uid")` 消费方无需改动。
- 已核实：cloud-core v1.1.7 中 uid **未被读取、未参与路径拼接** ⇒ 重建用户不影响其现有代码路径。

---

## 附录 B：参考

- [阿里邮箱 IMAP/POP/SMTP 服务器地址及端口配置（阿里云官方帮助中心）](https://help.aliyun.com/zh/document_detail/36576.html)
- [阿里企业邮箱如何设置域名 DNS 解析（开发者社区文章，取值以邮箱后台为准）](https://developer.aliyun.com/article/1697090)
- [企业邮箱账号每日发送额度（开发者社区问答，口径需自行核实）](https://developer.aliyun.com/ask/679124)

---

*本文档由代码审计结论推导而来，所有涉及现状代码的断言均可通过文中给出的 `文件:行号` 复核。*
