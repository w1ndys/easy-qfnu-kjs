#!/bin/sh
# 从备份集恢复到「显式指定」的目标库：pg_restore --clean --if-exists --no-owner。
#
# 定时备份（backup.sh）与恢复（本脚本）都只在 backup sidecar 里执行，连接参数来自
# compose 注入的 PG* 环境变量（PGHOST / PGUSER / PGPASSWORD），脚本不读 .env、不打口令。
#
# 设计约束（specs/2026-11-16-deploy-ops/design.md「恢复与密钥备份」）：
#   - TARGET_DB 与 BACKUP_FILE 必须显式给，缺一个就拒绝（TARGET_DB 为空时绝不回落到
#     PGDATABASE，防止误恢复生产库）；
#   - 目标库必须已存在，脚本不代建库；不存在时打印可直接复制的 createdb 命令；
#   - CONFIRM 必须逐字等于 yes，否则只打印目标与文件后拒绝；
#   - 全部校验通过才执行 pg_restore；stdout 只给开始/结束，stderr 只给错误。
#
# 用法（由 backup sidecar 提供 /scripts 与 /backup 两个挂载）：
#   docker compose -f deploy/compose.yaml --env-file .env run --rm \
#     --entrypoint /scripts/restore.sh \
#     -e TARGET_DB=easy_qfnu_kjs_restore \
#     -e BACKUP_FILE=/backup/kjs-20261116T082255Z.dump \
#     -e CONFIRM=yes backup
set -eu

TARGET=${TARGET_DB:-}
BACKUP=${BACKUP_FILE:-}
CONFIRM_VALUE=${CONFIRM:-}

log() { echo "restore: $*"; }
die() { echo "restore: 错误：$*" >&2; exit 1; }

# 1) 两个环境变量必须显式非空：缺哪个报哪个，不猜默认值。
if [ -z "$TARGET" ]; then
    die "缺少 TARGET_DB：必须显式指定目标库（不会默认恢复到 PGDATABASE）"
fi
if [ -z "$BACKUP" ]; then
    die "缺少 BACKUP_FILE：必须显式指定容器内备份文件（例如 /backup/kjs-20261116T082255Z.dump）"
fi

# 2) 确认门：只有逐字 yes 才继续。恢复带 --clean，会先删目标库里的同名对象，
# 所以拒绝时把「要动哪个库、用哪个文件」都打出来，便于运维核对后再带 CONFIRM=yes 重跑。
if [ "$CONFIRM_VALUE" != "yes" ]; then
    echo "restore: 拒绝执行：目标库 '$TARGET'，备份文件 '$BACKUP'" >&2
    echo "restore: 该操作会 --clean --if-exists 覆盖目标库现有对象；确认无误请显式传 CONFIRM=yes" >&2
    exit 1
fi

# 3) 目标库必须已存在：不去猜维护库、也不代建。用户输入按 SQL 字面量转义（单引号翻倍），
#    避免库名里的引号把查询拼坏。
TARGET_LITERAL=$(printf '%s' "$TARGET" | sed "s/'/''/g")
EXISTS=$(psql -d postgres -Atc "SELECT 1 FROM pg_database WHERE datname = '$TARGET_LITERAL'") ||
    die "查询目标库是否存在失败（检查 PGHOST/PGUSER 与 postgres 是否可达）"
if [ "$EXISTS" != "1" ]; then
    echo "restore: 错误：目标库 '$TARGET' 不存在；本脚本不代建库" >&2
    # 提示里的库名用单引号包住，含空格/特殊字符时也能直接复制执行。
    echo "先建库：createdb -U ${PGUSER:-postgres} '$TARGET'" >&2
    exit 1
fi

# 4) 备份文件必须是容器内存在且非空的普通文件：/backup 是宿主 ./backup 的 bind mount，
#    task deploy:restore 负责把宿主路径映射成容器内 /backup/<basename>。
if [ ! -f "$BACKUP" ]; then
    die "BACKUP_FILE 不存在：${BACKUP}（容器内路径；task deploy:restore 会把宿主 backup/xxx.dump 映射到 /backup/xxx.dump）"
fi
if [ ! -s "$BACKUP" ]; then
    die "BACKUP_FILE 为空：$BACKUP"
fi

# 5) 校验全过才动目标库。耗时只用于 stdout 的结束行，不参与任何判定。
START=$(date +%s)
log "开始恢复：$BACKUP → ${TARGET}（pg_restore --clean --if-exists --no-owner）"

pg_restore --clean --if-exists --no-owner -d "$TARGET" "$BACKUP"

END=$(date +%s)
log "恢复完成：${TARGET}（${BACKUP}，耗时 $((END - START)) 秒）"
