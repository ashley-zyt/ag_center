# 平台发布接口规则(API)

服务仅监听本机 `127.0.0.1:8080`（可通过 `LISTEN_ADDR` 覆盖），对外统一通过同机 nginx 反代的 HTTPS 域名访问，不直接暴露 IP:端口。所有发布接口均为 `POST /{platform}/publish`，`Content-Type: application/json`。

## 鉴权（所有接口通用，必读）

除 `/health` 外，**所有接口都必须携带以下 4 个请求头**，否则返回 `HTTP 401`。

| 请求头 | 说明 |
|---|---|
| `X-API-Key` | API 密钥，值 = 服务端环境变量 `API_KEY` |
| `X-Timestamp` | 当前 Unix 时间戳（秒），服务端允许 ±15 分钟时钟偏移（可用 `AUTH_MAX_SKEW_SECONDS` 调整） |
| `X-Nonce` | 每次请求随机生成的唯一串（建议 32 位 hex），服务端去重防重放 |
| `X-Signature` | HMAC-SHA256 签名（hex 小写），算法见下 |

**签名算法**：

```
bodyHash   = hex( sha256( 原始请求体字节 ) )            # 小写 hex
canonical  = METHOD + "\n" + PATH + "\n" + timestamp + "\n" + nonce + "\n" + bodyHash
signature  = hex( HMAC-SHA256( secret, canonical ) )    # secret = API_SECRET
```

- `PATH` 为请求路径（如 `/facebook/publish`），不含 query。
- `bodyHash` 必须对**发送到服务端的原始 body 字节**计算（即先序列化成字符串，再对该字符串哈希并原样发送）。
- `secret` = 服务端环境变量 `API_SECRET`（未设置时回退为 `API_KEY`，建议两者分开配置）。
- 时钟偏移默认 ±15 分钟；如服务端机器时钟未与调用方同步，可设环境变量 `AUTH_MAX_SKEW_SECONDS`（秒）放宽容差。⚠️ 放宽会同时放大重放攻击窗口，仅作临时方案，建议最终仍对齐双方时钟。

### curl 调用示例

```bash
API_KEY="你的API_KEY"
API_SECRET="你的API_SECRET"
BODY='{"profile_name":"fb001","title":"夏日促销","video_oss_url":"https://oss.example.com/a.mp4"}'
TS=$(date +%s)
NONCE=$(openssl rand -hex 16)
BODYHASH=$(printf '%s' "$BODY" | openssl dgst -sha256 | awk '{print $NF}')
SIGN=$(printf '%s\n%s\n%s\n%s\n%s' "POST" "/facebook/publish" "$TS" "$NONCE" "$BODYHASH" \
  | openssl dgst -sha256 -hmac "$API_SECRET" | awk '{print $NF}')

curl -X POST "https://你的域名/facebook/publish" \
  -H "Content-Type: application/json" \
  -H "X-API-Key: $API_KEY" \
  -H "X-Timestamp: $TS" \
  -H "X-Nonce: $NONCE" \
  -H "X-Signature: $SIGN" \
  -d "$BODY"
```

> 鉴权失败统一返回 `HTTP 401` + `{"type":"error","error_info":"..."}`，常见原因：密钥错误、时间戳超出 ±15 分钟、nonce 重复（重放）、body 被篡改导致签名不匹配。

---

## 通用规则(5 个平台一致)

所有发布接口流程统一:**启动 Profile → 调用平台 `PublishVideo` → 停止 Profile**。

- **视频来源二选一**(至少传一个):
  - `video_oss_url`:从 OSS 下载到本地临时文件后上传
  - `video_path`:本地文件路径,会转绝对路径并校验文件存在
- **必填字段**:`profile_name`、(`video_oss_url` 或 `video_path`)
- **可选缺省值**(在 `startProfileByName` 内回退):
  - `host` = `127.0.0.1`
  - `port` = `25325`
  - `wait_seconds` = `45`
  - `undetectable_path` = 空
- **统一响应字段**:`type` / `profile_id` / `debug_port` / `websocket_link` / `status` / `undetectable_host` / `undetectable_port` / `error_info`
- **成功 `status`**:固定 `publish_triggered`(仅表示发布动作已触发,非平台审核通过)

## 各平台差异

| 平台 | 路由 | 源码位置 | 文案字段 | 文案回退逻辑 | 额外字段 |
|---|---|---|---|---|---|
| Facebook | `POST /facebook/publish` | [main.go:1823](cmd/server/main.go#L1823) | `title` | 无回退,直接用 title | 无 |
| Twitter | `POST /twitter/publish` | [main.go:1926](cmd/server/main.go#L1926) | `text`、`title` | `text` 为空时回退到 `title` | 无 |
| YouTube | `POST /youtube/publish` | [main.go:2037](cmd/server/main.go#L2037) | `text`、`title`、`description` | `title` 为空时回退到 `text` | `description`(描述) |
| TikTok | `POST /tiktok/publish` | [main.go:2148](cmd/server/main.go#L2148) | `text`、`title` | `text` 为空时回退到 `title` | 无 |
| Instagram | `POST /instagram/publish` | [main.go:2258](cmd/server/main.go#L2258) | `text`、`title` | `text` 为空时回退到 `title` | 无 |

> 回退方向不同:
> - Twitter / TikTok / Instagram:**`text` 优先,空则用 `title`**
> - YouTube:**`title` 优先,空则用 `text`**
> - Facebook:只有 `title`
>
> YouTube / TikTok 在发布后多一个 `time.Sleep(8s)` 再停 Profile([main.go:2133](cmd/server/main.go#L2133)、[main.go:2242](cmd/server/main.go#L2242))。

## 请求体字段对照

✅ = 必填,✓ = 可选,— = 该平台不支持此字段

| 字段 | Facebook | Twitter | YouTube | TikTok | Instagram |
|---|:-:|:-:|:-:|:-:|:-:|
| `profile_name` ✅ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `title` | ✓ | ✓(回退源) | ✓(主) | ✓(回退源) | ✓(回退源) |
| `text` | — | ✓(主) | ✓(回退源) | ✓(主) | ✓(主) |
| `description` | — | — | ✓ | — | — |
| `video_oss_url` ✅二选一 | ✓ | ✓ | ✓ | ✓ | ✓ |
| `video_path` ✅二选一 | ✓ | ✓ | ✓ | ✓ | ✓ |
| `host` | 可选 | 可选 | 可选 | 可选 | 可选 |
| `port` | 可选 | 可选 | 可选 | 可选 | 可选 |
| `wait_seconds` | 可选 | 可选 | 可选 | 可选 | 可选 |
| `undetectable_path` | 可选 | 可选 | 可选 | 可选 | 可选 |

## 调用示例(curl)

```bash
# Facebook(只有 title)
curl.exe -X POST http://174.139.46.15:8080/facebook/publish -H "Content-Type: application/json" -d "{\"profile_name\": \"fb001\", \"title\": \"夏日促销\", \"video_oss_url\": \"https://oss.example.com/a.mp4\"}"

# Twitter(text 优先,可省略则用 title)
curl.exe -X POST http://174.139.46.15:8080/twitter/publish -H "Content-Type: application/json" -d "{\"profile_name\": \"tw001\", \"text\": \"新视频上线 #test\", \"title\": \"备用标题\", \"video_path\": \"D:/videos/tw.mp4\"}"

# YouTube(多 description,title 优先)
curl.exe -X POST http://174.139.46.15:8080/youtube/publish -H "Content-Type: application/json" -d "{\"profile_name\": \"yt001\", \"title\": \"视频标题\", \"description\": \"详细描述\", \"video_oss_url\": \"https://oss.example.com/b.mp4\"}"

# TikTok(text 优先)
curl.exe -X POST http://174.139.46.15:8080/tiktok/publish -H "Content-Type: application/json" -d "{\"profile_name\": \"tt001\", \"text\": \"caption 文案\", \"video_oss_url\": \"https://oss.example.com/c.mp4\"}"

# Instagram(text 优先)
curl.exe -X POST http://174.139.46.15:8080/instagram/publish -H "Content-Type: application/json" -d "{\"profile_name\": \"ig001\", \"text\": \"caption 文案\", \"video_path\": \"D:/videos/ig.mp4\"}"
```

## 响应示例

成功(HTTP 200):

```json
{
  "type": "success",
  "profile_id": "xxx",
  "debug_port": "9222",
  "websocket_link": "ws://127.0.0.1:9222/...",
  "status": "publish_triggered",
  "undetectable_host": "127.0.0.1",
  "undetectable_port": 25325
}
```

失败:

- HTTP 400(参数错误):`profile_name` 缺失、`video_oss_url`/`video_path` 都缺失、`video_path` 文件不存在、JSON 解析失败等
- HTTP 502(启动或发布失败):Profile 启动失败、平台发布流程异常

失败响应体:

```json
{
  "type": "error",
  "error_info": "具体失败原因"
}
```
