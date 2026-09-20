# Panel Migration PRD（workbuddy2api-gui → CPA workbuddy 插件面板）

> 项目：把独立 GUI（workbuddy2api-gui-master）的管理界面与可复用功能迁入 CPA workbuddy 插件面板，
> 继续通过 `/v0/resource/plugins/workbuddy/panel` 访问，由 CPA 主进程托管。
> 日期：2026-09-21。状态：已批准实施。

## 1. 背景与问题

- 旧插件面板（panel.html，单文件）功能聚焦：账号卡片、积分进度、签到/刷新/导入/屏蔽词。
- 独立 GUI（React+Vite）覆盖更全：总览仪表盘、账号表格（搜索/筛选/多选/详情）、批量任务进度、
  OAuth 登录向导、聊天测试台、统计、配置编辑、系统页。
- 但 GUI 的后端是独立 Go HTTP 服务（8787 端口、独立会话体系、直接读写 auths/ 与 config.json、
  docker.sock 控制容器）——这些与 CPA 插件架构不兼容，**只迁界面与交互，不迁后端**。

## 2. 目标与非目标

### 目标

1. 面板获得 GUI 的信息架构与视觉设计（仪表盘统计块、账号表格、批量任务进度条）。
2. 继续单文件部署（panel.html 经 go:embed），运行时零 Node 依赖、零额外端口。
3. 所有数据经 CPA 既有管理 API（<base>/plugins/workbuddy/*）获取；新增 API 走
   managementRegistration + handleManagement + 插件层鉴权。
4. 兼容三种打开方式：CPA iframe 嵌入、独立 URL、light/dark/white 主题。
5. 保留旧面板全部既有功能与 API 路径，零回滚。

### 非目标（明确排除）

- 不引入 React/Vite 构建链（评估结论：单文件 vanilla JS 完全能承载目标页面，React 收益不抵构建复杂度）。
- 不移植 GUI 的 internal/gateway、internal/authstore、internal/upstream、internal/ops 作为后端。
- 不实现 /status、/healthz、/v1/stats、/v1/stats/reset 的模拟。
- 不引入 docker.sock、容器重启、独立会话/密码体系、任务持久化数据库。
- 不直接读写 auths/ 或 CPA config.yaml。

## 3. 用户与场景

用户 = 插件管理员（持有 CPA management key）。场景：

- S1 打开面板 30 秒内看到：账号健康总览（healthy/cooling/disabled）、积分汇总、模型目录就绪状态。
- S2 在账号表格中搜索昵称/UID、按区域/状态筛选，对单账号发起签到/刷新/积分/选用/试炼。
- S3 对多账号（或全部）发起批量签到/保活/积分刷新，看到逐账号进度与结果。
- S4 通过向导添加账号（OAuth），看到授权 URL、轮询状态、成功后的账号信息。
- S5 查看模型目录就绪状态与最近保活/签到结果；切换自动签到开关。

## 4. 功能需求（按优先级）

| # | 需求 | 优先级 | 来源 |
|---|---|---|---|
| F1 | 仪表盘：统计块（账号数/健康/冷却/禁用/积分汇总/在途）+ 需关注账号列表 | P1 | GUI Dashboard |
| F2 | 账号表格：搜索（昵称/UID）、区域/状态筛选、单账号操作（签到/刷新/积分/选用/试炼）、积分明细展开 | P1 | GUI Accounts + 旧面板卡片 |
| F3 | 批量任务：多选/全选 → 批量签到/保活/积分 → TaskProgress 进度弹窗（1s 轮询改为客户端本地驱动） | P2 | GUI Accounts 批量 + TaskProgress |
| F4 | 登录向导：OAuth 设备授权三步流程（region 选择 → 授权 URL → 轮询 → 成功落盘） | P2 | GUI LoginWizard |
| F5 | 设置：自动签到开关、屏蔽词入口、保活状态卡 | P2 | 旧面板 + GUI 设置区 |
| F6 | 账号详情弹窗：完整字段 + 包列表（不做上游实时 buddy/travel 拉取） | P3 | GUI AccountDetail（裁剪） |
| F7 | 保留旧面板全部能力：导入凭证、屏蔽词设置、出口 IP、服务器时间 | P1 | 旧面板 |
| F8 | 聊天测试台 / 统计 / 价格 / 配置编辑 / 系统页 | **不做** | 见 §5 |

## 5. 暂不支持（记录于 API Contract，未来按 CPA 契约补齐）

| GUI 功能 | 不做原因 |
|---|---|
| 聊天测试台（Playground） | 面板是 management 域（管理密钥），聊天走客户端 api-key 域；跨域代理需要新造转发层，且与 CPA 主服务 chat 端点职责重叠。等 CPA 提供面板侧 chat 代理契约后再做 |
| 统计与趋势图（/v1/stats、TrendChart） | CPA 主服务的 stats 契约不稳定（插件内 usage 插件各自为政），凭空造代理会与主服务冲突 |
| 价格编辑（/api/pricing） | 依赖统计功能；无上游价格权威源 |
| 网关 config.json 编辑 | standalone 概念；CPA 侧只允许编辑插件自身配置（checkin/config、desensitize 已覆盖核心开关） |
| 系统页（容器状态/重启/改密） | 依赖 docker.sock 与第二套密码体系，均被架构约束禁止 |
| 猫猫旅行操作（单账号/批量 travel） | 旧插件（v0.9.3 基线）无 travel 管理端点；第 1 期融合 2api scheduler 后再评估 |
| 文件问题列表（file_issues）与凭据磁盘合并视图 | authstore 直读被禁止；host.auth.list 已含 status/disabled，够用 |
| 删除账号 | host 侧删除接口契约未确认前不做（避免误删）；import/禁用已够日常运维 |

## 6. 验收标准

1. `go build -buildmode=c-shared -o workbuddy.so .` 成功。
2. `go test -race ./...`、`go vet ./...` 通过；`gofmt -l` 不新增未格式化文件。
3. 既有路径全部保留可访问：panel、accounts、credits、checkin、import、keepalive 等 12 条。
4. 新增端点全部在 managementRegistration 声明、经 handleManagement 分发、受插件层鉴权与限流。
5. 面板在 iframe 嵌入与独立 URL 均可加载；主题三态正常。
6. 无新端口监听、无独立 HTTP server、无 auths//config.json 直读、无 docker.sock。
7. 所有错误响应脱敏（无 token、无本地绝对路径、无 management key 回显）。
8. 旧面板功能全部保留（导入凭证、屏蔽词、出口 IP、单账号操作、主题、嵌入逻辑）。
9. 批量任务有逐账号结果展示；失败账号不影响其他账号继续。

## 7. 回滚方式

- 代码层：git revert 本次迁移 commit（单 commit 或按阶段多 commit，均可独立 revert）。
- 部署层：服务器保留旧版 `workbuddy-v2.0.0-p0.so`（迁移前版本），回滚 = 恢复该 .so + restart。
- 数据层：本迁移不写任何持久化状态（无新文件、无新缓存结构），无需数据回滚。
