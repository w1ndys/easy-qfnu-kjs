# admin-bootstrap 实施计划

- [ ] 1. 迁移 0004 与管理数据模型
  - 新增 `migrations/0004_admin_session.sql`：`admin_session(token_hash PK, csrf_token, created_at, expires_at)`、`CHECK (expires_at > created_at)`、`admin_session_expires` 索引；只新增表，0001–0003 不动
  - 新增 `internal/model/auth.go`：`AdminUser`、`AdminSession`、`SessionTTL = 12h` 与 `ErrAdminExists`/`ErrKeyExists`/`ErrKeyUnavailable`/`ErrAdminStoreFailed`
  - 起库跑一次 `store.Migrate`，确认重复执行幂等、`schema_migration` 记到 0004

- [ ] 2. store.AdminRepo
  - 实现 `Admin`/`CreateAdmin`（`INSERT ... ON CONFLICT DO NOTHING`，返回 created 标志）/`DeleteAdmin`/`UpdatePassword`
  - 实现 `InsertSession`/`Session`/`DeleteSession`/`DeleteAllSessions`/`DeleteExpiredSessions`，SQL 只出现在该文件
  - 结构匹配 `auth.AdminStore`（store 不 import auth）；集成测试覆盖冲突、过期筛选与影响行数

- [ ] 3. auth 密码规则与 argon2id 哈希
  - `ValidatePassword`：两次不一致、少于 12 码点、超过 256 码点分别返回哨兵错误
  - `HashPassword` 产出 `$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>`，salt 16B、输出 32B，PHC 串自带参数
  - `VerifyPassword` 解析 PHC、按串内参数重算、`subtle.ConstantTimeCompare` 比对；损坏或截断哈希返回校验失败而非 panic
  - 并发哈希用容量 2 的信号量封顶，`HashCallCount` 供限流用例断言「被限流时不哈希」

- [ ] 4. secret 密钥文件
  - `NewManager`/`Load`：缺失返回 `(false, nil)`，不可读或内容非法返回 `ErrKeyUnavailable`
  - `Create`：父目录 `MkdirAll 0700`、`O_EXCL` 写入、文件 0600，已存在返回 `ErrKeyExists` 且不覆盖
  - 文件格式为 32 字节密钥的 base64url 文本 + 换行；`Export`/`Fingerprint` 与文件内容一致
  - `Load` 的判定口径：必须指向普通文件，且 `perm&0o077 == 0`（0400/0500/0600/0700 合规）。两类失败都返回 `ErrKeyUnavailable` 并按启动期降级处理，但文案分开写：权限过宽是「不得允许属主之外访问」（带实际权限位，不是固定 0600）；类型不符是「不是普通文件（目录/字符设备/块设备/命名管道/套接字）」。0070 在非 root 下由 `open(2)` 的 EACCES 先拦，走不到权限分支、没有权限文案，只保证 `ErrKeyUnavailable`
  - `Load` 读取封顶：有效文件恒为 44 字节，先按 `f.Stat()` 的 `Size()` 拒掉超过 44 字节的文件（路径误指大文件时不读内容、错误里写出真实大小；这一步在类型与权限判定之后），再以 `io.LimitReader(f, encodedKeyLen+2)`（45 字节）兜住 Stat 之后被追加的情形，读取量封顶
  - `IsRegular` 挡的是「打开之后去读」那一半，`open(2)` 对没有写入端的 FIFO 仍可能阻塞（残余风险，不为密钥路径引入平台相关的 `O_NONBLOCK`）
  - 单元测试覆盖创建 0600 权限、二次 Create、截断文件与指纹长度；权限判定表驱动（0400/0500/0600/0700 合规，0640/0604/0070/0644 拒绝）、非普通文件（目录、`/dev/null` 字符设备）、超大文件读取前拒绝、写入/落盘/关闭/清理失败（含 `open` 非 EEXIST 失败）、`Load` 与 `Create` 并发互斥

- [ ] 5. secret 密文信封
  - `Seal` 生成 12 字节随机 nonce，输出 `v1:` + base64url(nonce||ciphertext)，AAD 传入业务键名
  - `Open` 校验前缀、长度、AAD 与 GCM tag，任一项不符返回错误
  - 测试覆盖往返一致、两次 Seal nonce 不同、逐字节篡改失败、AAD 不匹配失败、未知版本前缀失败

- [ ] 6. auth 会话令牌、CSRF 与过期判定
  - `NewSessionToken` 产出 32B 随机原文与 SHA-256 摘要；`HashSessionToken` 供查询复用
  - `NewCSRFToken` 产出 32B 随机 base64url；`CompareCSRFToken` 走常量时间比对
  - `SessionExpiry` 恒为 `now+12h`；`SessionActive` 以 `now < expires_at` 判定，覆盖边界前一秒与后一秒

- [ ] 7. api 失败计数限流器
  - `Blocked`/`Fail`/`Reset`：窗口 60 秒、阈值 5 次失败，从该来源首次失败起算；`Retry-After` 落在 `(0, 60s]`
  - 成功登录清零；来源表上限 4096，满时先清过期再淘汰最旧，不影响其他来源
  - 复用 `clientSource`：可信反代下取 XFF 最右可解析跳，伪造最左值不换桶
  - 单测用假时钟覆盖第 6 次拦截、窗口到期恢复、清零与淘汰

- [ ] 8. api 管理中间件与错误码
  - 写请求先 `http.MaxBytesReader`（8 KiB）再要求 `application/json`，不符返回 40001
  - `withSession` 读 Cookie → 查 `admin_session` → `SessionActive` 判定，失败返回 40101 并清 Cookie
  - `withCSRF` 比对 `X-CSRF-Token`，失败返回 40301；两步都在业务逻辑之前，替身断言写方法调用为 0
  - 所有管理端响应（含错误）加 `Cache-Control: no-store`；错误码按设计表映射

- [ ] 9. GET /api/admin/state 与 POST /api/admin/init
  - `state` 返回 `{initialized, authenticated, expires_at, csrf_token, key_fingerprint}`，未登录时后三项为 null，并走公开 60 次/分钟限流
  - `init` 固定顺序：`Admin()` 已存在 → 40902 → 校验密码（40001/42201）→ `Reload()` 对账，密钥存在或不可用 → 40903 → 哈希 → `CreateAdmin` → `Create()` 写密钥；响应含一次性 `encryption_key`、`key_algorithm`、`key_fingerprint` 与 `Cache-Control: no-store`
  - `Create()` 失败时 `DeleteAdmin` 补偿并返回 50000，补偿失败记 `init_compensation_failed`；被拒请求不计算哈希、不写密钥文件（init 会 `Reload` 读一次）
  - 并发用例：50 个请求同时初始化，恰好 1 个 200、其余 40902，密钥文件只出现一次

- [ ] 10. 登录与退出
  - `POST /api/admin/session`：`VerifyPassword` 通过后 `InsertSession`（`expires_at=now+12h`），返回 `expires_at`/`csrf_token` 并下发 `HttpOnly`、`SameSite=Lax`、`Path=/api/admin`、`Secure` 随配置的 Cookie
  - 未初始化、密码错误、哈希损坏统一 40101 与同一文案；失败计入来源限流，达到阈值返回 42901 + `Retry-After`
  - 登录成功顺手 `DeleteExpiredSessions`，失败只记日志；成功时清零该来源失败计数
  - `DELETE /api/admin/session`：会话 + CSRF 校验通过后删记录并清 Cookie，返回 `{logged_out:true}`

- [ ] 11. 改密与全量会话失效
  - `POST /api/admin/password`：校验当前密码 → 新密码两次一致且 ≥12 码点 → `UpdatePassword` → `DeleteAllSessions`
  - 当前密码错误返回 40101 且不改哈希；当前密码只校验是否正确；新密码两次一致且 ≥12 码点（只验新密码，支持历史短密码），违反返回 42201
  - 成功后返回 `{relogin_required:true}` 并清 Cookie；改密前签发的任意 Cookie 后续请求返回 40101
  - 用例断言改密后 `admin_session` 行数为 0，且 `password_updated_at` 前移

- [ ] 12. 检查点：装配、脱敏与端到端
  - `main.go` 装配 `secret.NewManager(cfg.SecretKeyPath)`、`keys.Load()`、`store.NewAdminRepo(pool)`、`api.WithAdmin`、`api.WithAdminCookieSecure(cfg.AdminCookieSecure)`；config 增加 `SECRET_KEY_PATH`（空串=未设置=默认路径）与 `ADMIN_COOKIE_SECURE`（默认 true）；装配层加编译期断言 `var _ auth.AdminStore = store.AdminRepo{}`
  - 启动期密钥缺失检查：`adminRepo.Admin` 包 5s 超时，失败按 `admin_query_failed` / `secret_key_missing` 两条 WARN 继续启动，只带 `path` 不记底层错误
  - depguard 补 `auth`（`$gostd`、`model`、`x/crypto/argon2`）与 `secret`（`$gostd`、`model`）规则；`api` allow 补两个新包
  - 日志脱敏用例：用哨兵密码走完 init/login/改密，断言日志不含哨兵、Cookie 值、`csrf_token` 与 `encryption_key`
  - init 响应（经 handler 产生的 4xx/5xx 含 42901/40001/40902/40903/42201/50000）统一带 `Pragma: no-cache`；ServeMux 直接写出的 405 例外
  - 端到端：干净库迁移 → init → state → login → 带/不带 CSRF 写 → 改密 → 旧 Cookie 401 → 登出 → 清记录与密钥 → 重新 init；四条门禁全绿；有疑问询问用户
