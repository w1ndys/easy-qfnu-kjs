#!/bin/sh
# backup sidecar 的入口：装好 crontab 后前台跑 busybox crond。
#
# 为什么不把 crontab 单文件 bind mount 到 /etc/crontabs：crond 会校验 crontab 的属主与
# 权限，宿主侧权限差异（644、甚至属主不是 root）会让它拒读；所以这里从只读挂载的
# /scripts 拷一份 600 的到容器可写的 /etc/crontabs。
set -eu

CRONTAB_SRC=/scripts/crontab
BACKUP_SCRIPT=/scripts/backup.sh
# 用绝对路径，不依赖 PATH：postgres:17-alpine 里 busybox 的 crond 在 /usr/sbin。
CROND=/usr/sbin/crond

if [ ! -f "$CRONTAB_SRC" ]; then
    echo "backup: 缺少 $CRONTAB_SRC（deploy/backup 未挂到 /scripts）" >&2
    exit 1
fi

if [ ! -f "$BACKUP_SCRIPT" ]; then
    echo "backup: 缺少 $BACKUP_SCRIPT" >&2
    exit 1
fi

# 不可执行就直接失败：否则 crond 每天触发一次「Permission denied」，备份静默缺失。
if [ ! -x "$BACKUP_SCRIPT" ]; then
    echo "backup: $BACKUP_SCRIPT 不可执行（仓库里应为 100755）" >&2
    exit 1
fi

install -m 600 "$CRONTAB_SRC" /etc/crontabs/root

# 验收按这行 grep（逐字含 "backup schedule loaded"），同时把载入的规则打出来便于排障。
echo "backup schedule loaded: $(cat /etc/crontabs/root)"

exec "$CROND" -f -l 8 -c /etc/crontabs
