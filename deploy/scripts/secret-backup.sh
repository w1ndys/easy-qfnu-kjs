#!/bin/sh
# 导出加密密钥：命名卷里的 /var/lib/easy-qfnu-kjs/secret.key → 宿主 ./backup/secret/secret.key。
#
# 约束（specs/2026-11-16-deploy-ops/design.md「恢复与密钥备份」、Requirement 8.6）：
#   - 产物权限固定 600，目录用 umask 077 建；
#   - stdout 只打路径、权限与字节数，绝不打印、cat、xxd、head 文件内容，也不写日志；
#   - server 未运行 / 管理员尚未初始化（密钥不存在）都按失败退出并给出下一步提示；
#   - 从仓库根或任意 cwd 调用都能工作：按脚本自身路径定位仓库根再 cd 过去。
#
# 用法：task deploy:secret-backup（或 sh deploy/scripts/secret-backup.sh）
set -eu
umask 077

# 按脚本自身位置解析仓库根（本文件在 deploy/scripts/ 下），不依赖调用者的 cwd。
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
cd "$REPO_ROOT"

if [ ! -f .env ]; then
    echo "secret-backup: 错误：缺少 .env：先 \`task deploy:env\`（或 \`cp .env.example .env\`）并填 POSTGRES_PASSWORD" >&2
    exit 1
fi

# compose 项目目录是 deploy/，所以 -f 与 --env-file 都必须显式给（与 Taskfile 的 COMPOSE 同一口径）。
compose() {
    docker compose -f deploy/compose.yaml --env-file .env "$@"
}

KEY_IN_CONTAINER=/var/lib/easy-qfnu-kjs/secret.key
DEST=./backup/secret/secret.key

# 1) server 必须正在运行：容器停着时 `docker compose cp` 的报错太含糊，先判一次给明确提示。
if ! compose ps --status running --services server | grep -qx server; then
    echo "secret-backup: 错误：server 未运行，先 \`task up\`" >&2
    exit 1
fi

# 2) 密钥必须已存在且非空。首次部署、管理员尚未在 /admin 初始化时，这个文件本来就不存在，
#    这是正常状态而不是故障：提示先完成初始化，不要拿空文件冒充备份。
if ! compose exec -T server test -s "$KEY_IN_CONTAINER" 2>/dev/null; then
    echo "secret-backup: 错误：管理员尚未初始化，密钥不存在（${KEY_IN_CONTAINER}）" >&2
    echo "secret-backup: 先在管理端完成初始化生成密钥，再重跑本任务" >&2
    exit 1
fi

# 3) 导出到宿主 ./backup/secret/。cp 失败时清掉可能写了一半的目标文件，不留半截密钥。
mkdir -p ./backup/secret
if ! compose cp "server:$KEY_IN_CONTAINER" "$DEST"; then
    rm -f "$DEST"
    echo "secret-backup: 错误：从 server 容器复制密钥失败" >&2
    exit 1
fi

# 4) 权限固定 600（docker compose cp 会带上源文件权限，这里再收紧一次），并校验非空。
chmod 600 "$DEST"
if [ ! -s "$DEST" ]; then
    rm -f "$DEST"
    echo "secret-backup: 错误：导出的密钥文件为空（${DEST}）" >&2
    exit 1
fi

# 5) 只报路径、权限、字节数：任何情况下都不把密钥内容写进 stdout/stderr。
# 用八进制权限（600）而不是 ls -l 的字符串：后者在 macOS 会带 ACL 标记（-rw-------@），
# 不是纯粹的模式位。stat 的选项两平台不同，先试 BSD 写法再退回 GNU 写法。
if MODE=$(stat -f '%Lp' "$DEST" 2>/dev/null); then
    :
else
    MODE=$(stat -c '%a' "$DEST")
fi
SIZE=$(wc -c < "$DEST" | tr -d ' ')
echo "secret-backup: 已导出 $REPO_ROOT/backup/secret/secret.key（权限 ${MODE}，${SIZE} 字节）"
