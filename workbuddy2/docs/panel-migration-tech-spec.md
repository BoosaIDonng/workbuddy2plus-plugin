# Panel Migration Tech-Spec（技术方案）

> 对应 PRD：panel-migration-prd.md。日期：2026-09-21。

## 1. 总体架构决策

### 1.1 前端：保持单文件 vanilla JS（不引入 React）

评估结论：目标页面（Dashboard 统计块、账号表格、批量进度、登录向导、设置）在旧 panel.html 的
现有模式（innerHTML 模板 + 事件绑定 + fetch 轮询）下完全可以承载；React/Vite 引入的是构建链、
双产物同步（dist + embed）、以及与现有 `__WB_MANAGEMENT_BASE_PATH_JSON__`/主题同步/密钥获取
逻辑的重新接线成本。单文件部署是旧面板已验证的优势，保留。

结构改造（panel.html 内部）：

- 顶部 Tab 导航：`总览` | `账号` | `登录` | `设置`（替代当前单页滚动布局）。
- 设计 token 对齐 GUI styles.css 的变量体系（--bg/--card/--border/--accent/--ok/--warn/--err/--radius），
  但保留旧面板既有 light/dark/white 三主题机制（GUI 只有暗色；迁移视觉，不迁移其单一主题假设）。
- 保留并复用旧面板的：management key 获取三级回退（?key= → sessionStorage → 主面板 localStorage）、
  response envelope 解包（readPanelResponse/sanitizePanelErrors）、主题同步（__wbThemeSync）、
  401/403 处理与防封禁限速（rejectPanelAuth/authFailUntil）、toast 体系。

### 1.2 后端：新增 API 全部走既有分发骨架

每个新端点（见 API-Contract）：

1. `managementRegistration()` 声明路由（自动出现在 CPA 插件管理 UI）。
2. `handleManagement` switch 新增 case，统一 `mgmtJSONResponse` 包响应。
3. 鉴权全部交给宿主 remote-management 中间件：插件不自带密钥、不做限流（`mutatingManagementPath`/
   `checkManagementAuth`/`allowManagementRequest` 已删除——插件密钥会拒绝宿主密钥，而那是面板
   唯一能自动拿到的凭证）。
5. 需要 auth 的操作继续经 host.auth.list/get/save Host API（panelHostAuthList/hostAuthGetBundle/
   syncAuthNote 现有函数），绝不触碰文件系统。

### 1.3 批量任务：客户端驱动，不引入服务端任务框架

GUI 的 ops/tasks.go 是内存任务管理器（goroutine + TaskView + 1s 轮询）。插件侧等价实现：

- **服务端不做批处理端点**。前端拿到账号列表后，逐账号串行/受控并发（与旧面板
  lazyLoadCredits 相同模式：并发上限 3、间隔 200ms）调用既有单账号端点
  （/checkin、/keepalive、/credits）。
- TaskProgress 弹窗由前端驱动：每个账号一行，状态随该账号请求完成更新；
  失败不中断后续账号（与 GUI 语义一致）。
- 理由：插件进程内常驻任务框架会带来生命周期/内存治理负担（ CPA 宿主重启、插件 reconfigure），
  而逐请求转发 + 前端聚合在 N≤几十账号规模下体验无差、复杂度低一个量级。

### 1.4 登录向导：复用插件既有 OAuth 管理端点

oauth.go 已实现 start/poll/cancel（OAuth 设备授权流，写盘走 host.auth.save）。当前这些
能力暴露为 `handleStartLogin/handlePollLogin/handleCancelLogin` 但**未挂管理路由**。
本迁移把它们挂到 `/login/start|/login/poll|/login/cancel`（见 API-Contract），
前端向导只是把 GUI LoginWizard 的三步交互映射到这三个端点。不复制 GUI 的 upstream OAuth 客户端。

### 1.5 总览聚合：轻服务端 + 客户端补算

- `/accounts`（既有）已返回 accounts[] + summary（积分聚合）+ model_status + server_time。
- 新增 `/overview` 做薄聚合：在 /accounts 基础上补 `healthy/cooling/disabled/in_flight_full`
  计数与 `needs_attention[]`（disabled 或 exhausted 或有 error 的账号摘要），数据全部来自
  既有 dashboard 构建（不新增上游调用）。
- token 过期/即将过期计数：host.auth.list 的 status 字段 + wbAuth ExpiresAt（已有），在
  overview 内计算，不单独拉上游。

## 2. 新增/修改文件清单

| 文件 | 变更 |
|---|---|
| `panel.html` | 重构：Tab 布局 + GUI 视觉迁移 + 批量任务弹窗 + 登录向导；保留全部既有函数与 API 调用 |
| `panel.go` | 无结构性变更（buildDashboardEx 响应已含全部所需字段） |
| `management.go` | 新增路由 case：/overview、/login/start、/login/poll、/login/cancel；mutating 集合追加 |
| `overview.go`（新） | buildOverview：accounts + host list 状态聚合 |
| `management_overview_test.go`（新） | overview 聚合单测（healthy/cooling/disabled 分桶、空列表、error 账号归入 needs_attention） |
| `management_login_test.go`（新） | login 路由分发测试（方法不对→405、注册表含新路由、鉴权覆盖） |
| `panel_features_test.go` | 追加：新路由注册断言、panel.html 含新 Tab 与批量任务 DOM 断言 |
| `docs/panel-migration-{prd,tech-spec,api-contract}.md` | 新增 |

## 3. 安全设计（对应 PRD 约束）

1. 鉴权：新读端点纳入「配置了 management_key 才校验」的既有策略；写端点（login/start、
   login/cancel）强制走鉴权（mutating 集合）。与现有 12 条端点策略完全一致。
2. 脱敏：login/poll 成功响应只返回 uid/nickname/file 名（沿用 handlePollLogin 现有脱敏面，
   不新增字段）；overview 只含计数与昵称/uid8。
3. 输入校验：login/start 的 region 枚举校验（cn|global，非法→400）；poll/cancel 的 id 长度
   与字符白名单校验（`[A-Za-z0-9-]{1,64}`）。
4. 限流：所有新端点继承 per-IP token bucket（429 语义与既有端点一致）。
5. 不记录任何 secret 到日志（沿用现有 redactSecrets/truncateRedacted 管线）。

## 4. 兼容性保证

- 既有 12 条 API：路径、方法、响应字段零变更（新增字段只加在新增端点）。
- panel.html：保留全部现有 JS 函数名与行为（saveKey/load/filterRegion/checkin/refreshCredits/
  selectAuth/claimTrial/importAuth/checkinAll/claimTrialAll/toggleAuto/模态/主题同步/key 管理），
  新 UI 是在这些函数之上的呈现层改造 + 新增视图。
- 面板单测（panel.test.js 18 项）保持通过（涉及 innerHTML 结构的断言按新布局更新，语义不变）。

## 5. 测试计划

- 单元：overview 聚合分桶、login 路由分发与校验、注册表完整性（新增路由在册）、
  panel.html DOM 断言（Tab 存在、批量弹窗存在、登录向导三步存在）。
- 回归：全量 go test -race（8 包）、node --test panel.test.js、go vet、gofmt 对比基线。
- 构建：CGO c-shared .so 构建。
- 部署后端到端：/v1/models、流式对话（回归第 0 期验证）、新面板四 Tab 渲染、批量任务弹窗。

## 6. 回滚

单 commit（或按阶段少量 commit）；revert 后用服务器保留的迁移前 .so 即可，无持久化状态需要清理。
