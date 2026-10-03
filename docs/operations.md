# 运维手册（easy-qfnu-kjs）

> 对应设计决策：旧仓库 `easy-qfnu-kjs-legacy` 的 `docs/issue-29-rearchitecture-decisions.md`（Q1—Q171）。

> v2 起运行形态已变：本机 PostgreSQL + WebUI 管理面板。本章节的 systemd timer、环境变量配置、Vercel/Git 发布与回滚都是上一版实现，作为运维事实保留；重写后按 `docs/contract/` 再更新。

## 1. 定时采集

v2：采集时间由 WebUI 配置的 cron 表达式驱动（`settings.cron_expr`）；下面的 systemd timer 是上一版实现。

```ini
# ~/.config/systemd/user/easy-qfnu-collector.service
[Unit]
Description=easy-qfnu-kjs 快照采集

[Service]
Type=oneshot
WorkingDirectory=%h/github/w1ndys/easy-qfnu-kjs
ExecStart=%h/github/w1ndys/easy-qfnu-kjs/collector run
# 可选：飞书告警
Environment=FEISHU_WEBHOOK_URL=...
Environment=FEISHU_WEBHOOK_SECRET=...
```

```ini
# ~/.config/systemd/user/easy-qfnu-collector.timer
[Unit]
Description=每日 04:10 采集

[Timer]
OnCalendar=*-*-* 04:10:00 Asia/Shanghai
Persistent=true

[Install]
WantedBy=timers.target
```

```bash
systemctl --user daemon-reload
systemctl --user enable --now easy-qfnu-collector.timer
systemctl --user list-timers easy-qfnu-collector.timer
journalctl --user -u easy-qfnu-collector.service -n 50
```

规则：普通日期刷新当前周 + 未来 4 周；周日刷新全学期。寒暑假/非教学周照常运行但跳过发布，开学自动恢复。

## 2. 手动执行

```bash
go build -o collector ./cmd/collector
./collector collect           # 采集到仓库外暂存目录
./collector validate          # Schema/哈希/分组数量校验
./collector publish           # 写入本机快照库并验收 current
./collector run               # 完整流程
./collector run --dry-run     # collect + validate，不发布
```

v2：采集锁用 pg_advisory_lock（同一时刻至多一轮），运行状态与告警去重落库；下面是上一版实现的文件写法：
锁：`~/.local/state/easy-qfnu-kjs/collector.lock`（等待 5 分钟超时放弃）。
状态/告警去重：`~/.local/state/easy-qfnu-kjs/collector-state.json`（0600）。
日志：`logs/collector.log`（JSON 行，日切归档，保留 30 天；目录已被 gitignore）。

## 3. 发布与验收

- 自动提交信息：`data: snapshot <term> release <release_id>`；学期切换：`data: term switch <term>`。
- 发布前自动 `git fetch && git pull --ff-only`；工作区存在采集预期外的改动会终止并告警。
- 验收：GitHub check-runs（15s×40 次）→ 生产 `GET /api/v1/manifest` 直到 `release_id` 匹配（10s×30 次）→ 固定样例查询。
v2：发布验收改为在库内读回最新一次发布并抽查，不再走 HTTP 域名验收（`PUBLISH_BASE_URL` 已废弃）；下面是上一版实现：
- 验收失败：不自动回滚、保留当前生产部署，飞书告警后人工处理。

## 4. 飞书告警

v2：飞书 webhook 与签名在 WebUI 管理面板配置（`settings.feishu_webhook_url` / `feishu_secret`）；下面是上一版的环境变量写法：

- `FEISHU_WEBHOOK_URL`、`FEISHU_WEBHOOK_SECRET`（自定义机器人加签模式）。

触发：重试耗尽后的失败、校验拦截、学期切换、连续失败后首次成功。非教学周与普通成功不通知。告警不含账号/Cookie/Token/原始 HTML。

## 5. 回滚

- 数据回滚：`gh api repos/w1ndys/easy-qfnu-kjs/deployments` 找到上一个生产部署，Vercel Dashboard 对该 Deployment 点 Redeploy；或 `git revert` 上一个 `data:` 提交后推送 main（会触发新部署）。
- 域名回退：把 `kjs.easy-qfnu.top` 的 CNAME 指回旧 Docker 服务的入口，直到旧仓库服务下线。

## 6. Git 历史与归档

- 工作树只保留当前学期数据；历史保留在 Git 对象中。
- 仓库打包体积（`git count-objects -vH` 的 size-pack）：500 MB 告警、1 GB 停止自动发布。
- 到达 1 GB 的人工归档流程（不可进 cron）：
  1. 私有镜像备份：`gh repo create w1ndys/easy-qfnu-kjs-archive-backup --private` 并推送全量；
  2. 学期归档包：本地 `git bundle` 或 tar 保存旧学期 `data/`；
  3. `git filter-repo --path data/terms/<旧学期> --invert-paths` 删除旧学期历史；
  4. 核验（build + validate + diff 当前数据）后 `git push --force`。

## 7. 上线切换（首版）

1. Vercel 连接 `w1ndys/easy-qfnu-kjs`（私有），分支 `main` 为生产。
2. 开发分支部署 Preview 验收四个 API 与页面。
3. `main` 生产验收通过后，把 `kjs.easy-qfnu.top` CNAME 指向 Vercel（保留原 DNS 服务商）。
4. 观察稳定后，归档 `easy-qfnu-kjs-legacy`（仓库 Settings → Archive）。
