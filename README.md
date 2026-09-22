# WorkBuddy —— CLIProxyAPI 插件（二改增强版）

把**腾讯 CodeBuddy**（`www.codebuddy.cn` 国内版 / `workbuddy.ai` 国际版）作为 OAuth 提供商接入 [CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI) 的原生插件：按账号动态发现模型、完整流式执行、积分感知的账号调度、四类每日自动化任务，以及内置的七页管理面板。

> **本项目是二改（second-development）版本，不是上游插件。**
> 上游 `Sliverkiss/cpa-plugin` 已于 v0.9.3 停止维护。本仓库把
> [workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 更优秀的核心（SSE 规整、加权账号池、成长任务）移植进插件形态，并独立继续开发。

## 为什么要二改

上游插件的执行器会产生「对话莫名中断 / 模型不思考」这类问题，根因是它的 SSE 管线会丢帧和乱序。本仓库把那套管线整体换成 workbuddy2api 的逐帧规整器：

| 上游的问题 | 本仓库的修复 |
|---|---|
| 流式中断、思考内容丢失 | 白名单重建帧、error 帧透传、`tool_calls` 按 index 收敛名称、id 续传 |
| 没有账号级调度 | 三因子加权选号（积分比、到期偏好、闲置补偿）+ 模型级 6004 冷却 |
| 二进制里带着死代码 | 删除约 1700 行无引用的 scheduler/redisstore/session |
| 没有成长自动化 | 活跃地图、猫猫旅行、Token 保活、每日签到 |

## 功能一览

### 提供商与执行器

- **OAuth 登录** —— 多账号 `workbuddy-<uid>.json` 凭证文件，经宿主的 `host.auth.save` 写入。国内版与国际版共用一个插件、一个配置块。登录在 CPA 自己的界面完成；面板负责导入凭证，不自带登录流程。
- **动态模型目录** —— 从 WorkBuddy 发现每个账号的可用模型并按账号缓存，缺失的元数据由 [models.dev](https://models.dev) 补齐。可选 `models` 列表可完全替代自动发现。宿主侧的 `oauth-model-alias` / `oauth-excluded-models` 依然生效。
- **流式执行器** —— 兼容 OpenAI 的对话补全接口，流式（经 `host.stream.emit` 输出真实 SSE）与非流式（把 SSE 折叠成单个 completion）都支持。内置 `tool_choice` 归一化、Claude Code 模板清理、按 realm 注入系统消息、`prompt_cache_key` 注入。
- **Token 计数** —— 已实现 `executor.count_tokens`。

### 账号池与调度

- **三因子加权选号**（`scheduler_mode: credits`）—— 按剩余积分比例、积分到期紧迫度、闲置时长排序，取 Top-5 候选并带 100ms 防羊群间隔。
- **模型级 6004 冷却** —— 模型级限流只冷却该账号的该模型，并对齐上游给出的重置墙钟；同一账号的其他模型照常可用。
- **12153 三振判定** —— 单次 12153（多为网络抖动）不再杀号，连续 3 次才禁用。
- **熔断器** —— 连续 3 次 5xx 开启 30 分钟熔断，指数退避至多 6 小时。
- **积分耗尽硬冷却** —— 耗尽账号冷却到次日 04:00，等 09:00 签到恢复，而不是被反复选中并持续失败。

### 每日自动化（四类任务）

| 任务 | 时间（本地） | 做什么 |
|---|---|---|
| **每日签到** | 09:00、21:00 | 国内版账号签到，随后跑生命周期对账 |
| **Token 保活** | 22:00 | 刷新 access token，避免 Keycloak 离线会话过期 |
| **活跃地图** | 10:00 | 每账号发 5 条活跃上报，回读连登天数自检，再跑奖励链（礼物/补偿包 → 补签卡 → 连登档位领取 → 抽奖） |
| **猫猫旅行** | 09:00、21:00 | 无猫则领养，否则推进一趟旅行（idle 派出、arrived 领奖） |

每类任务都可单独开关（`checkin_auto`、`token_keepalive`、`activity_auto`、`travel_auto`），也可在面板手动触发。两个耗时扫描带 in-flight 守卫，重复点击返回 `409`，不会把上报风暴跑两遍。

### 积分生命周期

| 状态 | 国内版账号 | 国际版账号 |
|---|---|---|
| 积分 > 0 | 正常 | 正常 |
| 积分 = 0 | `disabled: true`（保留凭证文件） | **删除**凭证文件 |
| 签到恢复积分 | 自动重新启用 | 不适用（已删除） |
| 可领专家包 | 不适用 | 每账号可领一次 |
| 积分未知 | 不动（绝不误杀） | 不动 |

执行器遇到硬性积分错误（402、「积分不足」等）会立即触发该账号的对账。

### 管理面板（七页）

入口 `/v0/resource/plugins/workbuddy/panel`，从 CPA 侧边栏打开：

- **总览** —— 按状态统计账号数、积分汇总、今日签到进度、需要关注的账号列表。
- **用量** —— 官方账单数据按模型 / 按天（上海自然日）/ 按客户端聚合，另有零依赖的自绘 SVG 趋势图，支持预设或自定义日期区间。
- **账号** —— 每账号积分进度条、积分到期倒计时、套餐徽章、签到状态与连登天数、区域筛选、搜索排序、手动签到、领取专家包、导入凭证，以及「停用/恢复」开关（把账号摘出路由但不删凭证）。
- **请求日志** —— 每次执行器结局：模型、状态码、首字延迟（TTFB）、总延迟、token、脱敏错误。
- **任务记录** —— 每次自动化运行的结果与**获得的积分数**，并带累计总和。
- **模型中心** —— 完整上游目录：显示名、描述、上下文窗口、最大输出、推理档位、积分倍率、标签与能力。
- **设置** —— 自动化开关、保活状态、屏蔽词、接入信息。

面板由 CPA 同源提供，从同源 `localStorage`（或 `?key=` 参数）读取宿主的管理密钥并转发。**插件自身不校验任何密钥** —— 鉴权完全由 CPA 宿主负责。

## 快速开始

### 1. 安装

从 [Releases](https://github.com/BoosaIDonng/workbuddy2plus-plugin/releases) 下载对应架构的压缩包，解压后把 `workbuddy.so` 按平台目录约定放进 CPA 插件目录：

```
插件目录/
  linux-amd64/workbuddy.so    ← 64 位 x86 服务器（最常见的云主机）
  linux-arm64/workbuddy.so    ← ARM 架构服务器（如树莓派、AWS Graviton）
```

同一时刻只能有一个文件解析为插件 id `workbuddy` —— 该 id 是凭证类型路由键，两份不能并行加载。

### 2. 在 `config.yaml` 启用

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    workbuddy:
      enabled: true
```

### 3. 登录账号

在 CPA 自己的登录界面添加账号，插件会为每个账号写入一个 `workbuddy-<uid>.json` 到凭证库。也可以把其他部署导出的凭证粘贴进面板的导入对话框。

### 4. 调用

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5.3-flash",
    "messages": [{"role": "user", "content": "你好"}],
    "stream": true
  }'
```

## 配置参考

所有字段都可选，位于 `plugins.configs.workbuddy` 下：

```yaml
plugins:
  configs:
    workbuddy:
      enabled: true

      # 可选：权威模型 ID 列表。条目必须是单行 YAML 字符串。
      # 非空列表即完整目录：绕过 WorkBuddy 目录 HTTP 与缓存读写，
      # 但 models.dev 的元数据抓取、ETag、last-good 缓存照常生效。
      # 缺失 / null / [] 保持动态发现。
      models: []

      # 国内版账号每日签到（默认 true）。本地时间 09:00 与 21:00。
      checkin_auto: true

      # 积分生命周期：国内版耗尽禁用、国际版耗尽删除、签到恢复后重新启用（默认 true）。
      lifecycle_auto: true

      # 每日 22:00 刷新 access token（默认 true），避免 Keycloak 离线会话过期。
      token_keepalive: true

      # 每日 10:00 活跃地图任务（默认 true）：活跃上报、连登自检、成长奖励领取。
      activity_auto: true

      # 每日 09:00 与 21:00 猫猫旅行任务（默认 true）：领养、派出、到站领奖。
      travel_auto: true

      # 在 system/developer 提示词与工具名称/描述中向屏蔽词插入 U+200B（默认 false）。
      desensitize: false

      # 可编辑的字面词表。缺失则用内置 85 词；[] 表示空自定义列表。
      desensitize_terms: []

      # OAuth 请求档位：cli（默认）或 workbuddy（桌面端档位）。
      oauth_client_mode: "cli"

      # 优先探测国内版严格企业积分，再退回个人资源包（默认 false；国际版不变）。
      enterprise_credits: false

      # 账号选择（默认 "off"）：
      #   off     → 完全交给 CPA 内置调度
      #   credits → 由插件按三因子加权排序候选
      scheduler_mode: "off"
```

**鉴权**由 CPA 宿主负责：其 `remote-management` 中间件要求每个 `/v0/management/*` 请求携带管理密钥，并在多次失败后封禁 IP。插件刻意不自带密钥 —— 第二套密钥会拒绝宿主密钥，而那是面板唯一能自动拿到的凭证。

**代理**不支持插件级配置，所有出站请求跟随宿主路由。残留的 `proxy-url` 会在重配时显式报错，而不是被静默忽略。

模型别名与排除走 CPA 原生 `oauth-model-alias` / `oauth-excluded-models`，插件侧不重复实现。

## 模型目录

目录按账号维护，其就绪状态决定能否执行。只有 `ready` 与 `stale` 账号可执行；`not_started`、`loading`、`failed` 在所有执行器入口都以固定的脱敏 `not_ready` 响应（HTTP 503）拒绝，同时被调度排除。

动态模式下，账号的首次 `model.for_auth` 调用是带鉴权的引导边界：

1. WorkBuddy `GET /v3/config` 给出账号可用模型 ID 与 serving 字段。只有 HTTP 404 / 405 才回退到旧接口，其他失败不回退。
2. [models.dev](https://models.dev) 为 WorkBuddy 缺失的字段补权威元数据。它**不**决定账号权限，也不覆盖 WorkBuddy 的 serving 字段。
3. 响应先校验，通过后才替换持久缓存，并发布该账号的不可变目录。

无有效缓存的首次引导是 fail-closed：两个来源都必须抓取、校验并落盘成功，账号才进入 `ready`。后续进程启动仍会尝试两个刷新；若某来源刷新失败但持有有效 last-good 缓存，账号以该缓存进入 `stale`；两者都没有则 `failed`。

缓存位置（取自 `os.UserConfigDir()`，Linux root 即 `/root/.config/...`）：

```
<用户配置目录>/CLIProxyAPI/workbuddy/
  model-catalog/          ← 模型目录缓存
  data/                   ← 面板可观测量（请求日志/积分流水/任务记录/池状态）
```

设置环境变量 `WB2_DATA_DIR` 可覆盖数据目录。每个流上限 2000 行，写入走防抖 + 原子 tmp+rename。

## 面板接口

全部位于 `<MANAGEMENT_BASE_PATH>/plugins/workbuddy` 下，需要宿主管理密钥：

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/accounts` | 账号列表（轻加载：只读缓存） |
| POST | `/refresh` | 强制刷新 + 生命周期对账 |
| GET | `/overview` | 汇总卡片 + 需关注账号 |
| GET | `/usage?days=1..31` | 账单聚合 |
| GET | `/credits[?auth_index=]` | 单账号或全部账号的积分（含签到） |
| GET | `/requests?limit=` | 请求日志 |
| GET | `/ledger?limit=` | 积分流水 |
| GET | `/tasks?limit=` | 任务记录 |
| GET | `/models` | 完整上游模型目录 |
| GET | `/keepalive/status` | 保活计划与上次运行 |
| GET | `/egress-ip` | 出口 IP |
| GET | `/desensitize` | 生效的屏蔽词设置 |
| POST | `/checkin` | 单账号或全部签到 |
| POST | `/checkin/config` | 切换自动签到 |
| POST | `/keepalive` | 立即刷新 token |
| POST | `/import` | 导入凭证 JSON |
| POST | `/trial` | 领取国际版专家包 |
| POST | `/select` | 设置面板选中账号 |
| POST | `/account/toggle` | 停用/恢复账号 |
| POST | `/activity` | 立即跑活跃扫描（运行中返回 409） |
| POST | `/travel` | 立即跑旅行扫描（运行中返回 409） |

错误统一返回 `{"error": "<脱敏消息>"}`。凭证、token、上游原始响应体不会出现在任何响应里。

## 开发

需要 Go 1.26+（与 CPA 对齐）与 Node（跑面板测试）：

```bash
make build     # 编译当前平台的插件
make test      # 全量测试（含竞态检测）+ 面板测试
make lint      # gofmt + go vet
make release   # 交叉编译 linux-amd64 + linux-arm64 到 dist/
```

Windows 上经 Git Bash 构建需要路径转换绕行：

```bash
MSYS_NO_PATHCONV=1 docker run --rm \
  -v "//d/路径/workbuddy2":/src -w //src golang:1.26-bookworm \
  sh -c 'CGO_ENABLED=1 go build -buildmode=c-shared -o workbuddy.so .'
```

模块地图见 [docs/architecture.md](workbuddy2/docs/architecture.md)，完整工作流见 [docs/development.md](workbuddy2/docs/development.md)，更新历史见 [CHANGELOG](workbuddy2/CHANGELOG.md)。

## 兼容性

- **CPA**：基于 `CLIProxyAPI/v7` SDK 构建，插件 ABI 版本 1，Go 1.26。
- **realm 隔离**：同一个账号不要同时通过两个客户端登录国内版和国际版 —— Keycloak 会话是单点的，refresh token 会互相顶掉。

## 许可

MIT —— 见 [LICENSE](workbuddy2/LICENSE)。上游插件版权归 Sliverkiss；本二改部分的修改以相同条款发布。
