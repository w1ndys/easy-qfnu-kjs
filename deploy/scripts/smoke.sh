#!/bin/sh
# 部署冒烟（opt-in，`task deploy:smoke`）：验证「镜像真的能跑」，而不只是「能构建」。
#
# 为什么不能只加 `-p kjs-smoke` 就 up：
#   base 的 deploy/compose.yaml 写死了 container_name、两个卷的 name 与网络的 name。
#   compose 的固定名优先于项目名前缀，只改项目名会直接撞上正在运行的 easy-qfnu-kjs-*
#   栈，甚至把临时数据写进真实命名卷。所以这里额外生成一份 override，把 4 个容器名、
#   2 个卷名、1 个网络名全部改成 kjs-smoke-*，并在启动前用 `compose config` 反查一遍
#   （见「防呆」段）：漏改任何一处都拒绝启动，绝不碰本地栈。
#
# 口径：
#   - project 名 kjs-smoke，宿主端口固定 18081，只 `up -d --build web`（依赖链
#     server → postgres 自动起，backup 不启）；
#   - 不读真实 .env：--env-file .env.example + shell 覆盖占位口令（冒烟不需要真实口令）；
#   - 无论成败都在 trap 里 down -v --remove-orphans 并删临时目录，最后反查无残留。
#   - 断言按 design Test Strategy C 段：SPA 兜底、/api 404 不兜底、缓存头、探针。
#
# 用法：task deploy:smoke（或 sh deploy/scripts/smoke.sh）
set -eu

# 按脚本自身位置解析仓库根（本文件在 deploy/scripts/ 下），不依赖调用者的 cwd。
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
cd "$REPO_ROOT"

PROJECT=kjs-smoke
SMOKE_PORT=18081
BASE_URL="http://127.0.0.1:$SMOKE_PORT"
# `up` 会先阻塞到 server healthy（构建时间 + start_period 60s 都不在这段里），
# 240s 是站点起来之后到可用的上限。
WAIT_TIMEOUT=${SMOKE_WAIT_TIMEOUT:-240}

for cmd in docker curl awk grep; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        echo "smoke: 错误：缺少命令 $cmd" >&2
        exit 1
    fi
done

TMPDIR_SMOKE=$(mktemp -d "${TMPDIR:-/tmp}/kjs-smoke.XXXXXX")
OVERRIDE="$TMPDIR_SMOKE/override.yaml"
BODY="$TMPDIR_SMOKE/body"
HEADERS="$TMPDIR_SMOKE/headers"

# 与 Taskfile 的 COMPOSE 同一口径（-f + 显式 --env-file），只额外加 -p 与 override。
# 环境变量用 shell 导出：优先级高于 --env-file，所以 .env.example 里的空 POSTGRES_PASSWORD
# 不必改，真实 .env 也完全不参与（冒烟不读任何真实口令）。
export POSTGRES_PASSWORD=ci-placeholder
export BIND_ADDR=127.0.0.1
export SERVER_PORT="$SMOKE_PORT"
export IMAGE_TAG=smoke

compose() {
    docker compose -p "$PROJECT" \
        -f deploy/compose.yaml \
        -f "$OVERRIDE" \
        --env-file .env.example \
        "$@"
}

fail() {
    echo "smoke: 失败：$*" >&2
    echo "smoke: 下面是 web / server / postgres 的关键日志（tail 60）：" >&2
    compose logs --tail=60 web server postgres 2>&1 || true
    exit 1
}

# 清理必须与启动用同一套 -p / -f / override / --env-file：少给一个都会清到别的 project。
cleanup() {
    status=$?
    trap - EXIT INT TERM
    echo "smoke: 清理：down -v --remove-orphans（project=${PROJECT}）"
    if ! compose down -v --remove-orphans; then
        echo "smoke: 警告：down -v 失败，请手工清理 project=$PROJECT 的容器/卷/网络" >&2
        if [ "$status" -eq 0 ]; then status=1; fi
    fi
    left_containers=$(docker ps -a --filter "label=com.docker.compose.project=$PROJECT" --format '{{.Names}}' || true)
    left_volumes=$(docker volume ls --filter "label=com.docker.compose.project=$PROJECT" --format '{{.Name}}' || true)
    left_networks=$(docker network ls --filter "label=com.docker.compose.project=$PROJECT" --format '{{.Name}}' || true)
    if [ -n "$left_containers" ] || [ -n "$left_volumes" ] || [ -n "$left_networks" ]; then
        residual=$(printf '%s %s %s' "$left_containers" "$left_volumes" "$left_networks")
        echo "smoke: 警告：仍有 kjs-smoke 资源残留：$residual" >&2
        if [ "$status" -eq 0 ]; then status=1; fi
    else
        echo "smoke: 临时资源已清空（容器 / 卷 / 网络均无 project=$PROJECT 残留）"
    fi
    rm -rf "$TMPDIR_SMOKE"
    exit "$status"
}
trap cleanup EXIT INT TERM

cat > "$OVERRIDE" <<'YAML'
# smoke 专用 override（由 deploy/scripts/smoke.sh 生成到临时目录）。
# base 编排里这些名字都写死了；只改 -p 不会生效，必须在 override 里逐个覆盖。
services:
  postgres:
    container_name: kjs-smoke-postgres
  server:
    container_name: kjs-smoke-server
  web:
    container_name: kjs-smoke-web
  backup:
    container_name: kjs-smoke-backup
volumes:
  pgdata:
    name: kjs-smoke-pgdata
  secret:
    name: kjs-smoke-secret
networks:
  default:
    name: kjs-smoke-net
YAML

echo "smoke: 校验 override 合并结果（防止撞上本地栈/真实卷）"
MERGED=$(compose config)

require_in_config() {
    case "$MERGED" in
        *"$1"*) ;;
        *)
            echo "smoke: 防呆失败：合并后的 compose 配置里缺少 \"$1\"，拒绝启动" >&2
            exit 1
            ;;
    esac
}

reject_in_config() {
    case "$MERGED" in
        *"$1"*)
            echo "smoke: 防呆失败：合并后的 compose 配置仍含 \"$1\"，会撞上本地栈，拒绝启动" >&2
            exit 1
            ;;
        *) ;;
    esac
}

for name in web server postgres backup; do
    require_in_config "container_name: kjs-smoke-$name"
done
require_in_config "name: kjs-smoke-pgdata"
require_in_config "name: kjs-smoke-secret"
require_in_config "name: kjs-smoke-net"
reject_in_config "container_name: easy-qfnu-kjs-"
reject_in_config "name: easy-qfnu-kjs-pgdata"
reject_in_config "name: easy-qfnu-kjs-secret"
reject_in_config "name: easy-qfnu-kjs-net"
echo "smoke: 防呆通过：容器名 / 卷名 / 网络名全部落在 kjs-smoke-*，不含真实栈名字"

echo "smoke: 启动（project=${PROJECT}，宿主端口 ${SMOKE_PORT}，build web 并自动起 server/postgres）"
if ! compose up -d --build web; then
    fail "compose up -d --build web 失败"
fi

echo "smoke: 等待 $BASE_URL/ 可用（上限 ${WAIT_TIMEOUT}s）"
deadline=$(( $(date +%s) + WAIT_TIMEOUT ))
while :; do
    if curl -fsS -m 2 -o /dev/null "$BASE_URL/"; then
        break
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
        fail "等待 $BASE_URL/ 可用超时（${WAIT_TIMEOUT}s）"
    fi
    sleep 2
done
echo "smoke: 站点已可用"

# 断言小工具：curl 的失败不直接触发 set -e，交给断言打印失败项 + 日志。
http_get() {
    curl -sS -D "$HEADERS" -o "$BODY" -w '%{http_code}' "$1" || true
}

expect_code() {
    if [ "$2" != "$3" ]; then
        fail "$1：期望 HTTP $2，实际 $3"
    fi
    echo "smoke:     通过：$1 -> HTTP $3"
}

expect_body_contains() {
    if ! grep -qF -- "$2" "$BODY"; then
        echo "smoke:     失败详情：$1 的正文前 200 字节如下：" >&2
        head -c 200 "$BODY" >&2 || true
        echo >&2
        fail "$1：正文里找不到 \"$2\""
    fi
    echo "smoke:     通过：$1 -> 正文含 \"$2\""
}

expect_header() { # label 头名 期望值片段
    # 按「头名」取出那一行再判断值：Cache-Control 的值可能是 "no-cache, no-store"
    # 这种组合，不能把「头名 + 值」当连续字符串匹配。
    header_line=$(grep -i -- "^$2:" "$HEADERS" | head -n 1 | tr -d '\r' || true)
    if [ -z "$header_line" ]; then
        echo "smoke:     失败详情：$1 的响应头如下：" >&2
        cat "$HEADERS" >&2 || true
        fail "$1：响应头里没有 \"$2\""
    fi
    case "$header_line" in
        *"$3"*)
            echo "smoke:     通过：$1 -> 响应头 \"${header_line}\" 含 \"$3\""
            ;;
        *)
            echo "smoke:     失败详情：$1 的响应头如下：" >&2
            cat "$HEADERS" >&2 || true
            fail "$1：响应头 \"${header_line}\" 不含 \"$3\""
            ;;
    esac
}

N=0
begin_assert() {
    N=$((N + 1))
    echo "smoke: [$N/8] $*"
}

echo "smoke: 开始断言（design Test Strategy C 段）"

# 1) 首页由 web 镜像里的静态产物给出（不是 nginx 品牌页、不是 404）。
begin_assert "GET / 返回 200 且正文含 <div id=\"root\">"
code=$(http_get "$BASE_URL/")
expect_code "GET /" 200 "$code"
expect_body_contains "GET /" '<div id="root">'

# 2) 从首页 HTML 取真实 /assets/*.js 地址再下载：文件名含内容哈希，必须带一年期 immutable。
begin_assert "GET /assets/*.js 实际资源返回 200 且 Cache-Control 含 immutable"
asset_path=$(grep -oE '/assets/[A-Za-z0-9._-]+\.js' "$BODY" | head -n 1 || true)
if [ -z "$asset_path" ]; then
    fail "GET /：首页 HTML 里找不到 /assets/*.js 引用（前端产物没进 web 镜像？）"
fi
code=$(http_get "$BASE_URL$asset_path")
expect_code "GET $asset_path" 200 "$code"
expect_header "GET ${asset_path}（缓存头）" "Cache-Control" "public, max-age=31536000, immutable"

# 3) SPA 兜底：/admin 没有对应文件，必须由 nginx 兜 index.html（管理端前端路由接管），
#    且管理端 HTML 不能被任何一层缓存住。
begin_assert "GET /admin 返回 index.html（SPA 兜底）且 Cache-Control 含 no-store"
code=$(http_get "$BASE_URL/admin")
expect_code "GET /admin" 200 "$code"
expect_body_contains "GET /admin" '<div id="root">'
expect_header "GET /admin（缓存头）" "Cache-Control" "no-store"

# 4) index.html 与 SPA 兜底响应必须每次回源校验。
begin_assert "GET / 的 Cache-Control 含 no-cache"
code=$(http_get "$BASE_URL/")
expect_code "GET /（复验）" 200 "$code"
expect_header "GET /（缓存头）" "Cache-Control" "no-cache"

# 5) 探针经 web 反代到 server：宿主侧看到的是全链路状态。
begin_assert "GET /healthz 经反代返回 200"
code=$(http_get "$BASE_URL/healthz")
expect_code "GET /healthz" 200 "$code"

# 6)
begin_assert "GET /readyz 经反代返回 200"
code=$(http_get "$BASE_URL/readyz")
expect_code "GET /readyz" 200 "$code"

# 7) /api 下的未注册路径必须仍是 server 的 404，正文与直连逐字一致；被兜成 index.html
#    就说明反代边界破了（监控会把「路径写错」看成「接口通了」）。
begin_assert "GET /api/does-not-exist 返回 404 且正文逐字 404 page not found（不兜底）"
code=$(http_get "$BASE_URL/api/does-not-exist")
expect_code "GET /api/does-not-exist" 404 "$code"
api_body=$(cat "$BODY")
if [ "$api_body" != "404 page not found" ]; then
    fail "GET /api/does-not-exist：正文不是 \"404 page not found\"（实际：${api_body}），/api 被兜成 HTML 了"
fi
echo "smoke:     通过：GET /api/does-not-exist -> 正文逐字 \"404 page not found\""

# 8) 缺失的哈希资源返回 404，不能被兜成 index.html（否则浏览器把 HTML 当 JS 执行）。
begin_assert "GET /assets/does-not-exist.js 返回 404（不兜 index.html）"
code=$(http_get "$BASE_URL/assets/does-not-exist.js")
expect_code "GET /assets/does-not-exist.js" 404 "$code"

echo "smoke: 8/8 断言通过"
echo "smoke: 冒烟通过；临时资源由 trap 清理"
