# easy-qfnu-kjs

曲阜师范大学空教室查询系统，现已迁移为前后端分离的 Docker 部署架构。

## 部署架构

- 前端使用 `Vite + Vue 3` 构建，并以 `Nginx` 容器提供静态页面
- 后端使用 `Go + Gin` 提供 API 服务
- 前后端分别通过 Docker 容器运行，并通过 `app-net` 内部网络通信
- 对外入口由 `Traefik` 负责负载均衡、TLS 和反向代理
- 不再使用旧的 `task deploy`、SSH 上传、二进制直传或进程守护部署方式

## 快速开始

### 1. 准备环境变量

```bash
cp .env.example .env
```

然后编辑 `.env`，填写真实账号信息。

Docker 构建阶段已内置 Go 和 npm 国内镜像配置：

- Go: `https://goproxy.cn,direct`
- npm: `https://registry.npmmirror.com`

因此执行 `docker compose build`、`task build` 或 `task up` 时，会在容器构建阶段直接使用国内镜像安装依赖，不会改动宿主机上的全局配置。

可用变量如下：

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `QFNU_USERNAME` 或 `QFNU_USER` | 学号 | 无 |
| `QFNU_PASSWORD` 或 `QFNU_PASS` | 密码 | 无 |
| `OCR_URL` | OCR 服务基础地址，后端调用 `${OCR_URL}/ocr` 识别验证码 | 无 |
| `PORT` | 后端容器监听端口 | `8080` |
| `GIN_MODE` | Gin 运行模式 | `release` |

### 2. 确保 Traefik 网络存在

```bash
docker network create traefik-public
```

如果你的环境已经有 Traefik 使用中的公共网络，可复用现有网络。

### 3. 启动服务

```bash
docker compose up -d --build
```

启动后：

- `frontend` 容器负责提供页面，并通过 Nginx 反向代理 `/api` 到 `backend:8080`
- `backend` 容器仅加入内部网络，不直接暴露宿主机端口
- `Traefik` 通过 `frontend` 容器标签接入对外流量
- `frontend` 健康检查使用 `curl -f http://127.0.0.1/index.html`，避免因 BusyBox `wget` 差异导致 Traefik 无法发现可用后端

### 4. 查看状态

```bash
docker compose ps
docker compose logs --tail=200
```

## Docker 组件说明

### `docker-compose.yml`

- `frontend` 服务
  - 基于 `frontend/Dockerfile` 构建
  - 监听容器内 `80` 端口
  - 接入 `traefik-public` 与 `app-net`
  - 通过 Traefik label 暴露站点
- `backend` 服务
  - 基于根目录 `Dockerfile` 构建
  - 监听容器内 `8080` 端口
  - 只接入 `app-net`
  - 挂载 `./data` 和 `./logs` 持久化数据

### 前端反向代理

前端容器内的 `Nginx` 配置位于 `frontend/nginx.conf`，会把 `/api/` 请求转发给 `backend:8080`，从而实现容器内网通信，避免浏览器直接访问后端容器。

## 本地开发

如果需要本地联调而不是走容器：

```bash
# 安装依赖
task install

# 终端 1：启动后端
task backend-dev

# 终端 2：启动前端
task frontend-dev
```

此时前端开发服务器会通过 `frontend/vite.config.js` 中的代理，把 `/api` 请求转发到本地 `http://localhost:8080`。

## Task 命令

```bash
task env-init
task install
task build
task up
task down
task logs
task ps
```

这些命令用于本地开发和 Docker 工作流。

## 运维部署

`Taskfile.yml` 已增加开发运维相关任务，并统一设置为 `silent: true`。

统计大屏接口在无数据时间范围下也会返回空统计结果，不再因为 SQLite 聚合函数返回 `NULL` 而触发 500。

### 远程同步项目文件

```bash
task deploy HOST=1.2.3.4 PORT=22 USER=root DIR=/srv/app
```

该命令会通过 `rsync` 将项目文件同步到目标服务器目录，自动为远端 `scripts/ops/*.sh` 添加执行权限，并在上传完成后立即执行 `docker compose up -d --build` 重建并拉起容器。

部署过程中会输出分阶段日志，包括远端目录准备、文件上传、容器重建、容器状态展示与健康检查。若某个服务未启动、健康检查失败或超时，脚本会自动输出对应服务的 `docker compose ps` 状态和最近 100 行日志，便于直接排查。

## 运维脚本

- `scripts/ops/deploy.sh`：同步项目到远程目录并立即重建容器
