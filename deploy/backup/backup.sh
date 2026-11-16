#!/bin/sh
# 每日全量备份：pg_dump -Fc → 非空校验 → 原子落位 → 原子更新成功标记 → 保留策略。
#
# 定时任务（cron 03:15）与 `task deploy:backup` 共用这一份实现，不维护第二套 pg_dump。
# 连接参数全部来自 compose 注入的 PG* 环境变量，脚本本身不读 .env、也不打口令。
set -eu
umask 077

BACKUP_DIR=/backup
RETAIN_DAYS=${BACKUP_RETAIN_DAYS:-7}

# 失败（含 pg_dump 非零退出、非空校验失败）时不留半截临时文件。
# set -u 下用 ${TMP:-}：trap 建立时 TMP 可能还没赋值。
TMP=
trap 'rm -f "${TMP:-}"' EXIT

log() { echo "backup: $*"; }
die() { echo "backup: 错误：$*" >&2; exit 1; }

case "$RETAIN_DAYS" in
    '' | *[!0-9]*) die "BACKUP_RETAIN_DAYS 必须是正整数（当前 '$RETAIN_DAYS'）" ;;
esac
if [ "$RETAIN_DAYS" -lt 1 ]; then
    die "BACKUP_RETAIN_DAYS 必须 >= 1（当前 '$RETAIN_DAYS'）"
fi

TS=$(date -u +%Y%m%dT%H%M%SZ)
FILE="kjs-$TS.dump"

mkdir -p "$BACKUP_DIR"
log "开始备份 ${PGDATABASE:-}@${PGHOST:-} → $BACKUP_DIR/$FILE"

# 先写隐藏临时名，成功且非空才 mv 成正式文件名：/backup 里出现 kjs-*.dump 就等于一份
# 完整可恢复的备份，不会出现「文件名像备份、内容半截」。
TMP=$(mktemp "$BACKUP_DIR/.kjs-XXXXXX")

pg_dump -Fc -f "$TMP"

if [ ! -s "$TMP" ]; then
    die "pg_dump 产出为空（$TMP）：不落位、不更新标记、不执行保留策略"
fi

mv "$TMP" "$BACKUP_DIR/$FILE"
TMP=
SIZE=$(wc -c < "$BACKUP_DIR/$FILE")
log "已写入 $FILE（$SIZE 字节）"

# 标记与文件同源：同一行里放 UTC 时间戳与刚落位的文件名，先写 .tmp 再 mv，
# 读的一方（task deploy:health）永远看到完整一行，不会读到写了一半的内容。
STAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf '%s %s\n' "$STAMP" "$FILE" > "$BACKUP_DIR/.last_success.tmp"
mv "$BACKUP_DIR/.last_success.tmp" "$BACKUP_DIR/.last_success"
log "已更新 .last_success：$STAMP $FILE"

# 保留策略：删除窗口之前的备份集（-mtime +（保留天数-1）），但按 mtime 最新的那一份
# 无论是否落在候选里都保留——保证任何情况下目录里至少还有最近一次成功备份（CP7）。
LATEST=$(ls -t "$BACKUP_DIR"/kjs-*.dump 2>/dev/null | head -n 1 || true)
if [ -z "${LATEST:-}" ]; then
    LATEST="$BACKUP_DIR/$FILE"
fi

DELETED=0
for f in $(find "$BACKUP_DIR" -maxdepth 1 -name 'kjs-*.dump' -mtime +$((RETAIN_DAYS - 1))); do
    if [ "$f" = "$LATEST" ]; then
        continue
    fi
    rm -f "$f"
    DELETED=$((DELETED + 1))
done
log "保留策略：删除 $DELETED 份（保留 $RETAIN_DAYS 天，最新一份恒保留：$LATEST）"
