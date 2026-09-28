# lobsterai-cliproxy

把 **网易有道 LobsterAI（龙虾）** 封装成 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）动态插件，
任何 OpenAI 兼容客户端都能直接调用龙虾账号背后的模型。

上游协议来自社区逆向项目 [xinxinshuhao-create/lobsterai2api](https://github.com/xinxinshuhao-create/lobsterai2api)（MIT），
本插件把它的「独立反代服务」形态改写成 CPA 的 in-process provider 插件：

| 能力 | 说明 |
|---|---|
| `auth_provider` | 浏览器 OAuth 登录（自动起本地回调服务器）、token 刷新、auth 文件解析 |
| `model_provider` | 优先用 `GET /api/models/available` 动态拉取，失败回退内置静态表 |
| `executor` | 转发 `/api/proxy/v1/chat/completions`，流式 / 非流式都支持 |
| `quota_provider` | `GET /api/user/profile-summary` 提供账号剩余积分 |
| `management_api` | 面板侧边栏 `lobsterai` 页（额度 / 签到）+ 凭据备注自动写余额 + JSON 接口 |

多账号不再靠插件内部的池子：每个 CPA 凭据就是一路账号，由 CPA 调度器做轮转与冷却。

## 构建 / 安装

```bash
cd lobsterai-cliproxy
make install                 # 编译并拷贝到 $CPA_DIR/plugins/<os>/<arch>/
# 或指定 CPA 目录
make install CPA_DIR=/path/to/CLIProxyAPI
```

产物命名决定插件 ID：`lobsterai.dylib` → 插件 ID `lobsterai`，
在 CPA 的 `config.yaml` 中启用：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    lobsterai:
      enabled: true
      priority: 120
      force_stream: true
```

重启 CPA，日志出现下面两行即加载成功：

```
pluginhost: plugin loaded plugin_id=lobsterai
pluginhost: plugin registered plugin_id=lobsterai plugin_name=LobsterAI version=0.3.9
```

## 添加账号

两种方式都行：

1. **面板浏览器登录**：Management Center → OAuth 登录 → 「开始 LobsterAI 登录」。
   插件会在 CPA 进程内起一个 `127.0.0.1:<随机端口>` 回调服务器，把登录 URL 里的
   `redirect_uri` 指向它（龙虾登录页强制要求 loopback 回调）。两条落地路径都支持：
   - **本机浏览器**：直接点「打开链接」完成登录，回调自动被插件捕获；
   - **远程浏览器**：登录后地址栏会停在 `http://127.0.0.1:<port>/auth/callback?code=...`，
     把整条 URL 粘进面板的「回调 URL」框提交即可（host 落盘，插件读取并换 token）。
2. **导入已有 auth 文件**：把 lobsterai2api 的 `auths/lobsterai-<uid>.json`
   直接丢进 CPA 的 `auth-dir`，插件会自动识别（嵌套 / 扁平两种格式都支持），
   文件名保持 `lobsterai-<uid>.json`。

## 使用

```bash
# 列出模型（含龙虾的 deepseek-v4-pro / kimi-k2.7-code / glm-5.2 / MiniMax-M3 …）
curl http://localhost:8317/v1/models -H "Authorization: Bearer <api-key>"

# 非流式
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <api-key>" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"你好"}]}'

# 流式
curl -N http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <api-key>" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

Claude Code / 其它消费 Anthropic 协议的客户端，把 BaseURL 指到 `http://localhost:8317`（不带 `/v1`）即可。

## 在面板里看额度

插件会往 CPA Management Center 注入一个 **`lobsterai` 菜单**（插件菜单，路径 `/v0/resource/plugins/lobsterai/credits`），
页面直接列出每个龙虾账号的：

- 账号昵称 / uid、**当前剩余积分**、各积分批次（类型、剩余、到期日）、状态（正常 / 登录失效 / 查询失败）
- 顶部显示**全部账号合计**与**自动签到状态**（上次执行 / 已签数量 / 下次执行），
  「刷新额度」强制重新查询，「每日签到」手动补签（+100/天）

同时插件会把余额写进凭据的 `note` 字段（形如 `龙虾 · 剩余 64.52 积分`），
所以**凭据列表里也能直接看到当前额度**，不用打开额度页。

后台每 30 分钟自动刷新一次（可配置），所以列表里的数字基本是新的。

> 积分用尽时上游返回 `40201 / 免费额度已用完`，插件会把它归类成
> `insufficient_credit`，CPA 据此冷却该凭据；面板上的「每日签到」可以恢复额度。

### 每日自动签到

插件加载后约 30 秒做一次补签，之后按 `checkin_interval_minutes`（默认 30 分钟）
重试，直到每个账号当天签到成功；成功/已签后当天不再重复调用（上游本身也是幂等的）。
每个账号独立判断，所以多账号会一起签。

状态可以直接查：

```bash
curl -H "Authorization: Bearer <mgmt-key>" \
  http://localhost:8317/v0/management/plugins/lobsterai/credits | jq .auto_checkin
# => {"enabled":true,"last_run":"2026-09-21T20:11:31+08:00",
#     "last_result":[{"status":"already","note":"今天已签到", ...}],
#     "next_run":"2026-09-21T20:41:31+08:00"}
```

程序化读取：

```bash
# JSON（管理密钥）
curl -H "Authorization: Bearer <mgmt-key>" \
  http://localhost:8317/v0/management/plugins/lobsterai/credits | jq

# 强制刷新（会同时写回凭据备注）
curl -X POST -H "Authorization: Bearer <mgmt-key>" \
  http://localhost:8317/v0/management/plugins/lobsterai/credits/refresh

# 每日签到（可带 {"auth_index":"..."} 只签一个账号）
curl -X POST -H "Authorization: Bearer <mgmt-key>" \
  http://localhost:8317/v0/management/plugins/lobsterai/checkin -d '{}'
```

## 模型别名（避免误扣额度）

多个 provider 会提供**同一个模型 id**（例如 `deepseek-flash`：龙虾和你的 DeepSeek 中转都有；
`glm-5.3-flash`：龙虾和 CodeBuddy 都有）。客户端一调这个裸 id，CPA 按优先级选 provider，
很可能就落到龙虾上、并消耗龙虾额度。

做法：给龙虾的版本起别名，裸 id 让给别的 provider：

```yaml
      alias_models:
        - deepseek-flash
        - glm-5.3-flash
      alias_prefix: "lobsterai-"
```

效果：

| 客户端请求的 model | 实际落到 |
|---|---|
| `deepseek-flash` | 你的 ds（DeepSeek 中转） |
| `glm-5.3-flash` | workbuddy（CodeBuddy） |
| `lobsterai-deepseek-flash` | **lobsterai**（别名映射回上游 `deepseek-flash`） |
| `lobsterai-glm-5.3-flash` | **lobsterai** |

配套把插件优先级调低（`priority: 1`），重名时自动让位；模型独有的 id 不受影响。

## 配置项

| 键 | 默认 | 说明 |
|---|---|---|
| `upstream_base` | `https://lobsterai-server.youdao.com` | 上游 API 基址 |
| `login_portal` | `https://lobsterai.youdao.com` | 登录门户 |
| `client_version` | `2026.9.4` | 上报给上游的客户端版本；厂商抬门槛时改这里 |
| `proxy_url` | 空 | 插件级代理，留空走直连（有道在国内，直连通常更稳） |
| `force_stream` | `true` | 上游只接受 SSE，非流式请求由插件聚合 |
| `quota_refresh_minutes` | `30` | 后台刷新额度并写入凭据备注的间隔；`0` 关闭 |
| `checkin_auto` | `true` | **每日自动签到**（+100 积分/天），每号每天只签一次 |
| `checkin_interval_minutes` | `30` | 当天还没签到时多久重试一次（最小 5 分钟） |
| `alias_models` | 空 | 用别名暴露的上游模型 id 列表（避免与其他 provider 重名被误路由） |
| `alias_prefix` | `lobsterai-` | 别名前缀，最终 id 为 `<prefix><模型id>` |
| `exclude_models` | 空 | 直接隐藏并拒绝的模型 id（精确匹配或 `前缀*`） |

## 实现说明

- **登录**：`{portal}/portal#/login?source=electron&redirect_uri=http://127.0.0.1:<port>/auth/callback&state=<state>`，
  回调里拿到 `code` 后 `POST {upstream}/api/auth/exchange`（带 `uuid` / `firstKeyfrom` 指纹），
  成功后按 `{auth:{…},account:{…}}` 落盘，字段与 lobsterai2api 兼容。
- **刷新**：`POST {upstream}/api/auth/refresh`，失败按 `auth_expired` 抛错让 CPA 冷却该凭据。
- **流式**：走 `host.stream.emit` 异步推送；非 OpenAI 入口（Claude/Gemini）自动补 `data:` 帧。
- **错误分类**：余额不足 → `insufficient_credit`，登录失效 → `auth_expired`，便于 CPA 分别处理。

## 已知限制

- 登录回调绑定 `127.0.0.1`，CPA 与浏览器不在同一台机器时请走「导入 auth 文件」。
- 上游偶发 404/5xx 由 CPA 自身重试策略兜底；插件不内置账号池冷却状态机（CPA 已负责）。
- 上游活动接口有 `clientVersion` 门槛，被拒时把配置里的 `client_version` 调高即可。

## License

MIT
