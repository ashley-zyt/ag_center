# 消息模块接口汇总

服务地址统一为 `http://ip:8080`，所有接口均为 `POST`，`Content-Type: application/json`。

> **鉴权**：本模块接口与发布接口共用同一套鉴权，必须携带 `X-API-Key` / `X-Timestamp` / `X-Nonce` / `X-Signature` 四个请求头，签名算法详见 `API.md` 的「鉴权」章节。缺失或错误将返回 `HTTP 401`。

本模块共 2 个接口：

| 接口 | 作用 |
|---|---|
| `POST /accounts/send_single_message` | 主动发消息（单条） |
| `POST /accounts/check_reply` | 判断对方是否回复 + 获取回复内容 |

---

## 统一约定：对方参数 `target_url`

所有接口定位对方账号统一使用 `target_url` 字段。**调用端按平台自行填入不同内容**：

| 平台（`platform`） | `target_url` 里填什么 | 示例 |
|---|---|---|
| `twitter`（X） | 对方 username | `@ashly35856` 或 `ashly35856` |
| `tiktok` | 对方主页链接 | `https://www.tiktok.com/@xxx` |
| `instagram` | 对方主页链接 | `https://www.instagram.com/xxx` |
| `facebook` | 对方主页链接 | `https://www.facebook.com/xxx` |

> `account_name` 为**可选兼容字段**，正常情况下无需传；仅当需要显式补充对方账号名时才使用。服务端不负责从链接反解账号名，平台差异由调用端掌握。

---

## 1. 发消息（单条）

**接口**：`POST /accounts/send_single_message`

### 请求字段

| 字段 | 必填 | 说明 |
|---|---|---|
| `profile_name` | ✅ | 浏览器名称，如 `duwei18` |
| `platform` | ✅ | 平台名，见上表 |
| `target_url` | ✅ | 对方账号，按平台填 username 或主页链接（见上表） |
| `message_content` | ✅ | 消息内容 |
| `account_id` | ✅ | 账号 ID，>0，用于追踪 |
| `account_name` | 否 | 可选兼容字段，正常不传 |
| `passcode` | 否 | X 私信密码，默认 `1472`（仅 twitter 使用） |
| `host` / `port` / `wait_seconds` / `undetectable_path` | 否 | 浏览器启动参数，不传用默认值 |

### 调用示例

```powershell
# X（twitter）：target_url 放 username
curl.exe -X POST "http://174.139.46.15:8080/accounts/send_single_message" -H "Content-Type: application/json" -d '{"profile_name":"duwei18","platform":"twitter","target_url":"@ashly35856","message_content":"nice to meet you","passcode":"1472","account_id":263}'

# TikTok：target_url 放主页链接
curl.exe -X POST "http://174.139.46.15:8080/accounts/send_single_message" -H "Content-Type: application/json" -d '{"profile_name":"duwei18","platform":"tiktok","target_url":"https://www.tiktok.com/@xxx","message_content":"hi","account_id":264}'

# Instagram：target_url 放主页链接（发消息前会自动先关注对方）
curl.exe -X POST "http://174.139.46.15:8080/accounts/send_single_message" -H "Content-Type: application/json" -d '{"profile_name":"duwei18","platform":"instagram","target_url":"https://www.instagram.com/xxx","message_content":"hi","account_id":265}'
```

### 响应字段

```json
{
  "type": "success",
  "profile_id": "536d636d52bd31955902544cdfd61d",
  "account_id": 263,
  "status": "completed",
  "result": {
    "target_url": "@ashly35856",
    "status": "sent"
  },
  "error_info": ""
}
```

- `status`：`completed` / `failed` / `not_logged_in` / `error`
- `result.status`：`sent`（发送成功）/ `failed` / `not_logged_in`

---

## 2. 判断对方是否回复 + 获取回复内容

**接口**：`POST /accounts/check_reply`

### 请求字段

| 字段 | 必填 | 说明 |
|---|---|---|
| `profile_name` | ✅ | 浏览器名称 |
| `platform` | ✅ | 平台名 |
| `target_url` | ✅ | 对方账号，按平台填 username 或主页链接 |
| `account_id` | 否 | 账号 ID，用于追踪 |
| `account_name` | 否 | 可选兼容字段 |
| `passcode` | 否 | X 私信密码，默认 `1472` |
| `since_incoming_count` | 否 | **已废弃**，保留字段，不再使用 |
| `host` / `port` / `wait_seconds` / `undetectable_path` | 否 | 浏览器启动参数 |

### 调用示例

```powershell
# X（twitter）
curl.exe -X POST "http://174.139.46.15:8080/accounts/check_reply" -H "Content-Type: application/json" -d '{"profile_name":"duwei18","platform":"twitter","target_url":"@ashly35856","passcode":"1472","account_id":263}'

# TikTok
curl.exe -X POST "http://174.139.46.15:8080/accounts/check_reply" -H "Content-Type: application/json" -d '{"profile_name":"duwei18","platform":"tiktok","target_url":"https://www.tiktok.com/@xxx","account_id":264}'

# Instagram
curl.exe -X POST "http://174.139.46.15:8080/accounts/check_reply" -H "Content-Type: application/json" -d '{"profile_name":"duwei18","platform":"instagram","target_url":"https://www.instagram.com/xxx","account_id":265}'
```

### 响应字段

```json
{
  "type": "success",
  "profile_id": "...",
  "account_id": 263,
  "status": "completed",
  "reply_status": "replied",
  "has_reply": true,
  "reply_count": 2,
  "replies": [
    {
      "sender_name": "",
      "direction": "incoming",
      "content": "第一条回复",
      "sent_at": "5:11 AM",
      "observed_at": "2026-08-19T18:12:10+08:00"
    },
    {
      "sender_name": "",
      "direction": "incoming",
      "content": "第二条回复",
      "sent_at": "5:13 AM",
      "observed_at": "2026-08-19T18:12:10+08:00"
    }
  ],
  "checked_at": "2026-08-19T18:12:10+08:00",
  "error_info": ""
}
```

### 核心字段说明

| 字段 | 含义 |
|---|---|
| `reply_status` | `awaiting_reply`（对方还没回）/ `replied`（对方已回） |
| `has_reply` | `true` / `false`，等价于 `reply_status == "replied"` |
| `reply_count` | 本次返回的对方回复条数 |
| `replies` | 对方回复内容数组（时间正序：旧 → 新） |
| `replies[i].direction` | `incoming`（对方发来） |
| `replies[i].content` | 回复文本（已排除时间戳） |
| `replies[i].sent_at` | 页面时间原文（可能为空） |
| `replies[i].observed_at` | 本次查询观察到该消息的服务端时间 |
| `checked_at` | 本次查询的服务端时间 |

---

## 判断逻辑（check_reply 的核心）

看会话里最后一条消息是谁发的：

- 最后一条是**自己发的**（outgoing）→ `reply_status = "awaiting_reply"`，`has_reply = false`，`replies = []`
- 最后一条是**对方发的**（incoming）→ `reply_status = "replied"`，返回"我最后一次发言之后"对方发来的所有消息

**多条回复场景**：如果对方在我最后一次发言后连续回复了 N 条，`replies` 会原样返回这 N 条（`reply_count = N`，时间正序），不会丢条、不会只取最后一条。调用方直接按 `reply_count` 和 `replies` 遍历即可拿到全部新回复。

方向判断依据（各平台略有差异，均已在平台内部实现）：

- **X / Twitter**：自己发的消息容器 class 含 `justify-end`，对方发的含 `justify-start`
- **TikTok**：通过消息内头像链接 handle 与对方 handle 比对（`sender != partner` 为 outgoing）
- **Instagram**：通过消息文本 `div[dir="auto"]` 的 class 后缀判定（`xyk4ms5` = 我方 outgoing，`x18lvrbx` = 对方 incoming）

---

## 会话式往返的典型用法

```
1. send_single_message 发消息
2. check_reply 判断对方是否回复
   ├─ awaiting_reply → 对方还没回，可稍后再查
   └─ replied       → 拿到 replies 内容（可能多条）
3. 需要继续聊 → 再 send_single_message
4. 再 check_reply（无需维护基线，自动只返回最新一轮回复）
```
