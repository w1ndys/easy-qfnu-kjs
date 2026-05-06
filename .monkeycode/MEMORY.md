# 用户指令记忆

本文件记录了用户的指令、偏好和教导，用于在未来的交互中提供参考。

## 格式

### 用户指令条目
用户指令条目应遵循以下格式：

[用户指令摘要]
- Date: [YYYY-MM-DD]
- Context: [提及的场景或时间]
- Instructions:
  - [用户教导或指示的内容，逐行描述]

### 项目知识条目
Agent 在任务执行过程中发现的条目应遵循以下格式：

[项目知识摘要]
- Date: [YYYY-MM-DD]
- Context: Agent 在执行 [具体任务描述] 时发现
- Category: [代码结构|代码模式|代码生成|构建方法|测试方法|依赖关系|环境配置]
- Instructions:
  - [具体的知识点，逐行描述]

## 去重策略
- 添加新条目前，检查是否存在相似或相同的指令
- 若发现重复，跳过新条目或与已有条目合并
- 合并时，更新上下文或日期信息
- 这有助于避免冗余条目，保持记忆文件整洁

## 条目

### 项目技术架构
- Date: 2026-05-06
- Context: Agent 在执行 Vant 4 前端重构任务时更新
- Category: 代码结构
- Instructions:
  - 前端使用 Vue 3.5 (Composition API + `<script setup>`) + Vue Router 4.5 + Axios 1.12
  - UI 组件库为 Vant 4，通过 unplugin-vue-components + @vant/auto-import-resolver 实现按需引入
  - 构建工具为 Vite 7，样式通过 Vant 主题变量覆盖 + scoped CSS 实现，不再使用 Tailwind CSS
  - 前端构建产物输出到 `dist` 目录
  - 主色调为 `#884F22`（棕色系），通过 `--van-primary-color` 覆盖 Vant 默认主题
  - 项目包含 6 个路由页面：首页、空教室查询、教室全天状态、数据大屏、管理登录、公告管理
  - 10 个组件：AppHeader(NavBar), AppFooter, DateSelector, ConfirmDialog(Dialog), EmptyState(Empty), LoadingSpinner(Loading), StatusWarning(NoticeBar), StatsCard, AnnouncementCard, QRCodeCard
  - 7 个 Composables：useAlertDialog, useAnnouncements, useBuildingAliasReminder, useDateSelection, useSearchHistory, useSystemStatus, useTopBuildings
  - CSS 变量定义在 `src/assets/css/main.css` 中，覆盖 Vant 主题变量并定义项目自定义变量

### 速率限制中间件
- Date: 2026-04-10
- Context: Agent 在执行搜索接口限流功能时发现
- Category: 代码模式
- Instructions:
  - 速率限制中间件位于 `internal/middleware/ratelimit.go`
  - 使用 IP + User-Agent 组合作为限流 key，通过 SHA256 哈希 UA 避免 map key 过长
  - 内存 map + sync.Mutex 实现，无第三方依赖
  - 后台协程定期清理过期条目，防止内存泄漏
  - 在 main.go 中以路由级中间件方式应用，仅作用于 `/api/v1/query` 和 `/api/v1/query-full-day`

### 用户主色调偏好
- Date: 2026-03-24
- Context: 用户在设计改造需求中明确指出
- Instructions:
  - 主体颜色必须保持为 `rgb(136, 79, 34)`，即 `#884F22`
  - 设计方案中的紫色系主色调需替换为此棕色系

### 部署方式迁移要求
- Date: 2026-04-24
- Context: 用户在说明当前系统部署方案迁移时明确指出
- Instructions:
  - 系统改为使用 Docker 部署，前后端分别由 Docker 容器运行
  - 前后端通过 Docker 网络进行内网交互
  - 下游统一由 Traefik 负责负载均衡与反向代理
  - 不再使用旧的 task deploy 流程
  - 不再通过二进制文件和进程守护方式运行服务

### 运维 Taskfile 要求
- Date: 2026-04-25
- Context: 用户在补充开发运维能力时明确指出
- Instructions:
  - 需要增强 `Taskfile.yml` 以承载开发运维任务，并统一设置 `silent: true`
  - 国内镜像源应优先在 Docker 构建容器内配置，不修改宿主机上的 Go 与 npm 全局镜像设置
  - `task deploy` 需支持通过 CLI 变量传入 `HOST`、`PORT`、`USER`、`DIR` 等远程部署参数
  - 部署逻辑继续通过独立 `sh` 脚本实现，便于后续复用
  - 运维任务中只保留 `task deploy`，不再保留 `prod-deploy` 别名
  - 当前部署方式全面迁移到 Docker 运行，不再保留 `systemd` / `systemctl` 相关脚本、任务和示例
  - 前端健康检查优先使用 `curl -f http://127.0.0.1/index.html`，避免 Traefik 因容器误判不健康而返回 404

### Docker 挂载目录权限约定
- Date: 2026-04-25
- Context: Agent 在执行统计数据库只读问题修复时发现
- Category: 环境配置
- Instructions:
  - 后端容器以非 root 用户 `app` 运行，但 `./data`、`./logs` 由宿主机 bind mount 后可能变成 root 拥有
  - 启动入口需要先修正 `/app/data` 与 `/app/logs` 的属主，再以 `app` 身份启动应用，避免 SQLite 报 `attempt to write a readonly database`
  - 统计数据库路径支持通过环境变量 `STATS_DB_PATH` 覆盖，默认仍为 `data/stats.db`

### Git 与回复偏好
- Date: 2026-04-25
- Context: 用户在说明后续协作与提交规范时明确指出
- Instructions:
  - 涉及 `gh` 登录时，可优先尝试复用现有 Git 凭据完成认证
  - commit message 使用专业格式：`改动类型(改动文件): 改动内容`
  - 提交前需一次性阅读所有改动，并按改动分类拆分为多次 commit
  - 提交说明需使用单行 `git commit` 命令，通过多个 `-m` 参数拼接内容
  - Git 提交相关信息应尽量在中文环境下展示
  - 提交信息里的贡献者 name 使用 `W1ndys`，邮箱使用 `w1ndys@qq.com`
  - 异常处理需尽量完善，并将报错信息显式反馈给用户
  - 聊天回复中的链接不得使用代码块包裹，必须使用纯文本或 Markdown 链接
  - 对于开发任务，开始修改代码前就要创建并推送新分支，并尽早发起 PR；后续每完成一个步骤都应及时推送
  - 若用户要求提交，优先使用中文环境展示 Git 信息

### 前端设计规范输出约定
- Date: 2026-05-06
- Context: Agent 在执行 Vant 4 前端重构任务时更新
- Category: 代码模式
- Instructions:
  - 该项目前端使用 Vant 4 作为移动端组件库，不再使用 Tailwind CSS
  - 页面结构由首页、空教室查询、全天状态、数据大屏、管理登录、公告管理六个路由组成
  - 品牌主色继续沿用 `#884F22`，通过 Vant CSS 变量 `--van-primary-color` 覆盖
  - 组件样式通过 scoped CSS + Vant 主题变量实现，不使用原子化 CSS 类
  - 全局样式文件 `src/assets/css/main.css` 负责覆盖 Vant 主题变量和定义项目级 CSS 变量
  - 支持亮色/暗色主题切换，通过 `[data-theme="dark"]` 选择器覆盖变量
