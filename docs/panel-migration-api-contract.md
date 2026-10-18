# Panel Migration API Contract

> 基准路径：`<MANAGEMENT_BASE_PATH>/plugins/workbuddy`（宿主注入，运行时从
> `__WB_MANAGEMENT_BASE_PATH_JSON__` 获取，禁止硬编码 `/v0/management`）。
> 鉴权：由 CPA 宿主负责——宿主的 remote-management 中间件要求每个 `/v0/management/*`
> 请求携带管理密钥，多次失败后封禁 IP。插件不再自带第二套密钥，也不做限流：面板从宿主
> 同源 `localStorage["cli-proxy-auth"]`（或 `?key=`）取密钥并作为 Bearer 转发。
> 错误信封：非 2xx 一律 `{"error": "<脱敏消息>"}`；成功为域对象（无包裹层）。

## 1. 既有端点（本次零变更，列全以防回归）

| Method | Path | 用途 | 变更 |
|---|---|---|---|
| GET | /desensitize | 屏蔽词运行时配置 `{enabled, terms[], source}` | 无 |
| GET | /accounts | 账号列表（wbAccount[] + summary + model_status + server_time） | 无 |
| GET | /egress-ip | 出口 IP `{ip}`（502 时 `{"error":"egress IP unavailable"}`） | 无 |
| POST | /refresh | 强制刷新全部账号缓存（= buildDashboardEx(force=true)） | 无 |
| POST | /checkin | 手动签到 `{auth_index?}`，缺省=全部 | 无 |
| POST | /checkin/config | 自动签到开关 `{enabled}` | 无 |
| GET | /credits | 实时积分 `?auth_index=`（单）或缺省（全部） | 无 |
| POST | /import | 导入凭证 JSON `{json?, raw?}`（nested/flat 双形态） | 无 |
| POST | /trial | Global 专家包领取 `{auth_index}` | 无 |
| POST | /select | 选用路由账号 `{auth_index}` | 无 |
| POST | /keepalive | 手动保活 `{auth_index?}` | 无 |
| GET | /keepalive/status | 最近保活摘要 + 配置 | 无 |

wbAccount 字段（不变）：`auth_index, auth_id, name, label, nickname, uid, region("cn"|"global"),
plan, status, disabled, exhausted, selected, credits?{total_remain,total_used,total_size,pack_count,
fetched_at,packages[]}, checkin?{active,today_checked_in,streak_days,daily_credit,...}, trial_claimed?, error?`。

## 2. 新增端点（本次引入）

### 2.1 GET /overview

聚合总览（数据全部来自既有 dashboard 构建与 host 状态，不新增上游调用）。

```jsonc
{
  "total": 3,
  "healthy": 2,            // !disabled && !exhausted && error==""
  "cooling": 0,            // status 含冷却语义的账号数（当前以 error/状态字段推断）
  "disabled": 1,
  "exhausted": 0,
  "in_flight_full": 0,     // 预留：宿主未透出时恒 0
  "credits": {"remain": 0, "used": 0, "size": 0, "known": 0, "packs": 0},  // summarizeCredits 口径
  "needs_attention": [     // disabled || exhausted || error 非空 的账号摘要
    {"auth_index": "...", "nickname": "...", "uid8": "12345678", "region": "cn",
     "reasons": ["disabled", "error"], "error": "..."}   // error 已脱敏
  ],
  "server_time": "2006-01-02 15:04:05"
}
```

映射自 GUI `GET /api/overview`；GUI 的 gateway_ok/warnings/file_issues/sticky_sessions/redis_mode
无插件等价物，不返回（见 §3）。

### 2.2 POST /login/start

发起 OAuth 设备授权（复用插件既有 handleStartLogin，不新增上游逻辑）。

- 请求：`{"region": "cn" | "global"}`（缺省 "cn"；其他值 → 400 `{"error":"invalid region"}`）
- 成功 200：`{"session_id": "...", "auth_url": "https://...", "region": "cn", "expires_in": 900}`
- 失败：502 `{"error": "<脱敏上游错误>"}`（session 已存在冲突→409）

映射自 GUI `POST /api/login/start`。

### 2.3 POST /login/poll

轮询授权状态。

- 请求：`{"session_id": "<id>"}`
- 成功 200（pending）：`{"status": "pending"}`
- 成功 200（complete）：`{"status": "success", "uid": "...", "nickname": "...", "file": "workbuddy-<uid>.json"}`
- 200（terminal）：`{"status": "expired" | "error" | "cancelled", "message": "<脱敏>"}`
- 校验：session_id 必须匹配 `[A-Za-z0-9-]{1,64}`，否则 400；未知 id → 404

映射自 GUI `POST /api/login/{id}/poll`（路径参数改请求体，避免 host 路径路由歧义）。
**不返回** token/凭证 JSON（GUI 的 saved/file 语义保留，secret 永不出插件）。

### 2.4 POST /login/cancel

- 请求：`{"session_id": "<id>"}`（校验同上）
- 成功 200：`{"status": "cancelled"}`
- 未知 id → 404

映射自 GUI `POST /api/login/{id}/cancel`。

## 3. GUI→CPA 映射总表

| GUI API | CPA 插件端点 | 状态 |
|---|---|---|
| GET /api/session | （面板内 management key 三级获取） | 机制替换 |
| POST /api/login | （无第二套密码） | 机制替换 |
| GET /api/overview | GET /overview | **新增**（裁剪） |
| GET /api/accounts | GET /accounts | 已有 |
| GET /api/accounts/{uid} | GET /accounts（行内数据）+ /credits?auth_index= | 已有组合 |
| POST /api/accounts/{uid}/checkin | POST /checkin {auth_index} | 已有 |
| POST /api/accounts/{uid}/refresh | POST /keepalive {auth_index} | 已有（语义对齐） |
| POST /api/accounts/{uid}/credits | GET /credits?auth_index= | 已有 |
| POST /api/accounts/import | POST /import | 已有 |
| POST /api/tasks/* | 前端逐账号调用单账号端点 | 客户端聚合 |
| GET /api/tasks/{id} | （TaskProgress 弹窗本地状态） | 客户端聚合 |
| POST /api/login/start | POST /login/start | **新增** |
| POST /api/login/{id}/poll | POST /login/poll | **新增**（body 传 id） |
| POST /api/login/{id}/cancel | POST /login/cancel | **新增**（body 传 id） |
| GET /api/models | CPA 宿主 /v1/models（不经插件） | 复用宿主 |
| GET /api/stats、/v1/stats/*、/api/pricing | — | 不做（无稳定契约） |
| POST /api/chat、/api/chat/stream | — | 不做（鉴权域分离） |
| GET/PUT /api/config、/api/config/reset | POST /checkin/config、GET/PUT desensitize（已有 config PATCH） | 部分已有，不做全量 |
| GET /api/system、/api/system/restart、/api/password | — | 不做（docker/密码被禁） |
| DELETE /api/accounts/{uid} | — | 不做（host 删除契约未确认） |
| POST /api/accounts/{uid}/travel、/api/tasks/travel | — | 不做（插件无 travel 端点；第 1 期评估） |

## 4. 校验与错误码约定

| 场景 | 状态 | body |
|---|---|---|
| 缺/错 Bearer（配置了 key） | 401 / 403 | `{"error": "missing Bearer token"}` / `{"error":"invalid management key"}` |
| 限流 | 429 | `{"error":"rate limit exceeded, try again later"}` |
| region 非法 | 400 | `{"error":"invalid region"}` |
| session_id 非法/未知 | 400 / 404 | `{"error":"invalid session id"}` / `{"error":"unknown session"}` |
| 未知路径 | 404 | `{"error":"not found: <path>"}`（既有行为） |
| 上游失败 | 502 | `{"error":"<脱敏摘要>"}` |

脱敏基线：错误消息不得包含 accessToken/refreshToken/management key/本地绝对路径/原始认证 JSON；
沿用 redactSecrets/truncateRedacted。

## 5. 验收核对清单（对应本 Contract）

- [ ] 12 条既有端点响应结构与迁移前逐字段一致（以面板功能与现有测试为证）。
- [ ] 4 条新端点在 managementRegistration.routes 中可见。
- [ ] login/start、login/cancel 在 mutatingManagementPath 中（无 key 时也要求宿主鉴权）。
- [ ] region/session_id 校验有单测。
- [ ] 429/401/403 行为与既有端点一致。
