-- 0004 管理会话：登录态由服务端持有，库里只留令牌摘要。
--
-- 0001 只有 admin_user，没有存放登录态的地方；本表补上。每次登录落一行，
-- 认证走主键查询，退出与改密可以立即删行收回会话（Requirement 5、CP8）。
-- 只存令牌的 SHA-256：拿到数据库也换不出可用 Cookie 原文（CP5）。
--
-- 不记 IP 与 User-Agent：单管理员场景用不上，加上反而要本 spec 定义它们的
-- 留存与脱敏口径；排障需要的来源信息只进日志（Requirement 9）。
CREATE TABLE admin_session (
  token_hash  text PRIMARY KEY,           -- 会话令牌的 SHA-256（hex），明文只在浏览器 Cookie
  csrf_token  text NOT NULL,              -- 同步器令牌：写操作经 X-CSRF-Token 回传
  created_at  timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,
  -- 过期时刻必须晚于创建时刻：反向或相等的行永远不可能是有效会话，
  -- 与其让认证时判定失败，不如在写入这一刻就挡掉（CP6）。
  CONSTRAINT admin_session_expiry_after_created CHECK (expires_at > created_at)
);

-- 登录成功顺手清理过期会话、运维兜底清理都按过期时刻筛。
CREATE INDEX admin_session_expires ON admin_session (expires_at);

COMMENT ON TABLE admin_session IS
  '管理会话：token_hash 为会话令牌的 SHA-256，明文令牌只存在于浏览器 Cookie';

COMMENT ON COLUMN admin_session.created_at IS
  '会话建立时刻，由应用写入当前时刻；DEFAULT now() 只兜底手工插入';

COMMENT ON COLUMN admin_session.expires_at IS
  '绝对过期时刻，恒为创建时刻 + 12 小时，不随访问滑动续期';
