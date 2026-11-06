# Requirements Document：admin-bootstrap

## Introduction

本 spec 交付 `easy-qfnu-kjs` 管理端的**引导与鉴权后端**：首次初始化、密码规则与不可逆存储、业务秘密加密密钥的生成与一次性导出、管理登录、12 小时服务端会话、CSRF 防护、登录失败限流、改密与重置，以及这些能力必须遵守的脱敏与路由边界。

本 spec **只做后端 HTTP API**。前端页面（React 19 + antd 6）留给后续的 `admin-web` / `public-web`；设置项读写、手动同步、版本对比与回退、告警记录留给 `admin-settings-sync` 与 `admin-release`。

本 spec 从 `specs/2026-10-27-classroom-availability-rewrite/requirements.md` 的 Requirement 20（管理员初始化）、Requirement 21（管理会话）、Requirement 30（安全与隐私）抽取，并按已谈定的业务边界收紧其中两处：**密钥只在初始化响应里出现一次**（原 Requirement 20.4 允许后续再次导出），**会话改为服务端存储**（原 spec 只规定 12 小时会话与 Cookie，未规定存续形态）。口径以本文件为准，`docs/prd.md` 5.1 与 7.4 需随本 spec 一并校正。

## Glossary

- **管理员**：系统唯一的管理账号，账号固定为 `admin`，在 `admin_user` 表中至多一行。
- **初始化**：在不存在管理员记录时，设置管理员密码并生成加密密钥的一次性动作。
- **管理会话**：登录成功后由服务端记录、由浏览器 Cookie 引用的登录状态，有效期 12 小时。
- **会话令牌**：登录成功时下发的 32 字节随机值，仅以 Cookie 原文形态存在于客户端，服务端只保存其 SHA-256。
- **CSRF 令牌**：与一条管理会话绑定的 32 字节随机值，写操作必须经 `X-CSRF-Token` 头回传。
- **加密密钥**：初始化时生成一次的 32 字节对称密钥，用于加密存储业务秘密（教务密码、飞书密钥等）。
- **密钥文件**：部署内保存加密密钥的文件，路径由 `SECRET_KEY_PATH` 指定；创建时权限 0600，读取时只要求属主之外不可访问（`perm&0o077 == 0`，0400/0500/0600/0700 合规，0644 这类开放属主之外访问的权限一律视为密钥不可用）。路径必须指向普通文件，目录、FIFO、设备文件、套接字一律视为密钥不可用；符号链接按目标文件处理——`Load` 用 `os.Open` 打开，链接在打开时已被跟随，类型与权限判定落在目标文件上。
- **密钥指纹**：加密密钥 SHA-256 的前 16 个十六进制字符，用于核对备份副本，不能还原密钥。
- **业务秘密**：管理员录入后只写不读的敏感配置值，本 spec 只交付其加解密能力，读写接口属于 `admin-settings-sync`。
- **失败限流窗口**：按来源计数的固定 60 秒窗口，从该来源首次失败时刻起算，登录与初始化各自计数。
- **来源**：限流与日志口径里的客户端标识，规则与公开查询一致（可信反代下取 `X-Forwarded-For` 最右可解析跳，否则取直连对端）。
- **PHC 哈希串**：`$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>` 形态的自描述密码哈希。

## 范围与非目标

范围：`POST /api/admin/init`、`POST /api/admin/session`、`DELETE /api/admin/session`、`GET /api/admin/state`、`POST /api/admin/password` 五个端点，配套的迁移、仓储、哈希、会话、CSRF、限流、密钥与加解密能力。

非目标：任何前端页面与静态资源；设置项、轮次、版本、告警等管理端业务端点；采集链路改动；公开查询 API 改动；多管理员、多角色、找回密码邮件或短信。

## Requirements

### Requirement 1 管理员初始化

**User Story:** AS 运维，I want 首次部署后自己设管理员密码，so that 不依赖环境变量预设凭据。

#### Acceptance Criteria

1. WHEN 系统不存在管理员记录，系统 SHALL 通过 `POST /api/admin/init` 接受初始化请求。
2. WHEN 初始化请求携带的两次密码一致且码点数不少于 12，系统 SHALL 创建账号固定为 `admin` 的管理员记录并返回成功。
3. WHEN 管理员记录已存在，系统 SHALL 以业务码 `40902` 拒绝初始化请求，并保持既有记录不变。
4. WHEN 两个初始化请求并发到达，系统 SHALL 让其中一个返回成功、其余返回 `40902`。
5. WHEN 初始化成功，系统 SHALL 让 `GET /api/admin/state` 返回 `initialized=true` 与 `authenticated=false`。

### Requirement 2 密码规则与不可逆存储

**User Story:** AS 管理员，I want 密码有硬性下限且只以哈希入库，so that 库被看到也不等于账号被拿走。

#### Acceptance Criteria

1. WHEN 两次提交的密码内容不一致，系统 SHALL 以业务码 `42201` 拒绝请求。
2. WHEN 密码的 Unicode 码点数少于 12，系统 SHALL 以业务码 `42201` 拒绝请求。
3. WHEN 初始化或改密成功，系统 SHALL 把 argon2id 哈希串写入 `admin_user.password_hash`，该串以 `$argon2id$v=19$m=19456,t=2,p=1$` 开头。
4. WHEN 校验密码，系统 SHALL 以哈希串自带参数重算摘要并做常量时间比对，判定依据是比对结果。
5. WHEN 已存的哈希串无法解析，系统 SHALL 判定本次校验失败并返回 `40101`。

### Requirement 3 加密密钥与一次性导出

**User Story:** AS 管理员，I want 初始化时拿到一次密钥并立即备份，so that 已存秘密在换机器后还能解开。

#### Acceptance Criteria

1. WHEN 初始化成功，系统 SHALL 生成 32 字节随机加密密钥并以权限 0600 写入密钥文件。
2. WHEN 返回初始化响应，系统 SHALL 在 `encryption_key` 字段返回该密钥的 base64url 明文，并在 `key_algorithm` 字段声明 `AES-256-GCM`。
3. WHEN 返回初始化响应，系统 SHALL 设置 `Cache-Control: no-store`。
4. WHEN 密钥文件已存在，系统 SHALL 以业务码 `40903` 拒绝初始化请求，并保持既有密钥内容与管理员记录不变。
5. WHEN 会话有效的管理员请求 `GET /api/admin/state`，系统 SHALL 返回密钥指纹，供管理员核对备份副本。
6. 明文密钥 SHALL 只在初始化响应中出现一次。

### Requirement 4 管理登录

**User Story:** AS 管理员，I want 用密码登录管理端，so that 只有我能进管理接口。

#### Acceptance Criteria

1. WHEN 管理员提交密码登录，系统 SHALL 以固定账号 `admin` 校验该密码。
2. WHEN 密码校验通过，系统 SHALL 建立管理会话并在响应中返回 `expires_at` 与 `csrf_token`。
3. WHEN 密码校验失败，系统 SHALL 返回业务码 `40101` 与文案「凭据无效或会话已过期」。
4. WHEN 系统尚未初始化，系统 SHALL 对登录请求返回业务码 `40101` 与同一段文案。

### Requirement 5 管理会话

**User Story:** AS 管理员，I want 登录态可被服务端立即收回，so that 泄露的 Cookie 有止期。

#### Acceptance Criteria

1. WHEN 登录成功，系统 SHALL 在 `admin_session` 写入一条过期时刻等于创建时刻加 12 小时的会话记录。
2. WHEN 收到携带会话 Cookie 的请求，系统 SHALL 以服务端记录判定有效性，判定条件为当前时刻早于 `expires_at`。
3. WHEN 请求携带的会话已过期或记录不存在，系统 SHALL 返回业务码 `40101`。
4. WHEN 管理员退出登录，系统 SHALL 删除对应会话记录并清除会话 Cookie。
5. WHEN 下发会话 Cookie，系统 SHALL 设置 `HttpOnly`、`SameSite=Lax`、`Path=/api/admin`，并在 `ADMIN_COOKIE_SECURE=true` 时设置 `Secure`。

### Requirement 6 CSRF 防护

**User Story:** AS 管理员，I want 写操作必须带只有本页能拿到的令牌，so that 别人伪装的请求动不了系统。

#### Acceptance Criteria

1. WHEN 处理管理端写操作（`POST /api/admin/password`、`DELETE /api/admin/session`），系统 SHALL 校验请求头 `X-CSRF-Token` 与会话记录中的令牌一致。
2. WHEN 写请求没有有效会话，系统 SHALL 返回业务码 `40101` 并清除会话 Cookie。
3. WHEN 写请求的 CSRF 令牌缺失或与会话记录不一致，系统 SHALL 返回业务码 `40301` 并保持业务数据不变。
4. WHEN 会话有效的客户端请求 `GET /api/admin/state`，系统 SHALL 返回当前会话的 CSRF 令牌。
5. WHILE 一条会话有效，系统 SHALL 为该会话返回同一个 CSRF 令牌。
6. WHEN 写请求的 `Content-Type` 不是 `application/json`，系统 SHALL 返回业务码 `40001`。

### Requirement 7 登录失败限流

**User Story:** AS 运维，I want 密码猜错被限速，so that 在线爆破在时间上不成立。

#### Acceptance Criteria

1. WHEN 同一来源在 60 秒窗口内累计 5 次登录失败，系统 SHALL 对该来源的下一次登录请求返回业务码 `42901`。
2. WHEN 记录一次登录失败，系统 SHALL 从该来源本次窗口的首次失败时刻起算 60 秒。
3. WHEN 登录成功，系统 SHALL 把该来源的失败计数清零。
4. WHEN 返回业务码 `42901`，系统 SHALL 在 `Retry-After` 头返回剩余秒数，取值不小于 1。
5. WHEN 判定来源，系统 SHALL 复用公开查询的来源规则：可信反代下取 `X-Forwarded-For` 最右可解析跳，否则取直连对端。
6. WHEN 同一来源在 60 秒窗口内累计 5 次初始化失败，系统 SHALL 对该来源的后续初始化请求返回业务码 `42901`。

### Requirement 8 改密与重置

**User Story:** AS 管理员，I want 改密后所有旧登录立刻失效，so that 密码换了就真的换了。

#### Acceptance Criteria

1. WHEN 管理员提交当前密码、两次一致且码点数不少于 12 的新密码，系统 SHALL 先校验当前密码再写入新哈希。
2. WHEN 当前密码校验失败，系统 SHALL 返回业务码 `40101` 并保持密码哈希与会话不变。
3. WHEN 改密成功，系统 SHALL 删除 `admin_session` 的全部记录。
4. WHEN 改密成功，系统 SHALL 在响应中返回 `relogin_required=true` 并清除会话 Cookie。
5. WHEN 运维删除 `admin_user` 记录与密钥文件，系统 SHALL 让 `GET /api/admin/state` 返回 `initialized=false` 并接受新的初始化请求。

### Requirement 9 脱敏与路由边界

**User Story:** AS 运维，I want 日志与响应只带排障必需字段，so that 凭据不会顺着日志流出去。

#### Acceptance Criteria

1. WHEN 输出管理端日志，系统 SHALL 只记录 `request_id`、HTTP 方法、路径、HTTP 状态码与业务码。
2. WHEN 返回管理端响应，系统 SHALL 让 `data` 只使用这些字段：`initialized`、`authenticated`、`expires_at`、`csrf_token`、`encryption_key`、`key_algorithm`、`key_fingerprint`、`relogin_required`、`logged_out`。
3. WHEN 注册路由，系统 SHALL 让公开路由只接受 `GET`，并把管理端写操作全部注册在 `/api/admin/` 前缀下。
4. WHEN 写入管理数据，系统 SHALL 只使用 `admin_user` 与 `admin_session` 两张表。
5. WHEN 返回管理端响应（含错误响应），系统 SHALL 设置 `Cache-Control: no-store`。
