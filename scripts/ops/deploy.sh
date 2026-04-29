#!/usr/bin/env bash

set -euo pipefail

log() {
  printf '[deploy] %s\n' "$1"
}

fail() {
  printf '[deploy] ERROR: %s\n' "$1" >&2
  exit 1
}

run_remote() {
  local message="$1"
  local command="$2"

  log "$message"
  ssh "${SSH_OPTS[@]}" "${REMOTE}" "cd '${DIR}' && printf '[remote] %s\\n' '$message' && ${command}" \
    || fail "$message failed"
}

if [ "$#" -ne 4 ]; then
  printf 'Usage: %s HOST PORT USER DIR\n' "$0" >&2
  exit 1
fi

HOST="$1"
PORT="$2"
USER="$3"
DIR="$4"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
REMOTE="${USER}@${HOST}"

SSH_OPTS=(
  -p "${PORT}"
  -o BatchMode=yes
  -o StrictHostKeyChecking=accept-new
)

IMAGE_FRONTEND="easy-qfnu-kjs-frontend"
IMAGE_BACKEND="easy-qfnu-kjs-backend"
TAR_FILE="easy-qfnu-kjs-images.tar"
LOCAL_TAR="/tmp/${TAR_FILE}"

# ---------- 本地构建镜像 ----------
log "building frontend image locally"
docker build -t "${IMAGE_FRONTEND}:latest" -f "${ROOT_DIR}/frontend/Dockerfile" "${ROOT_DIR}/frontend" \
  || fail "frontend image build failed"

log "building backend image locally"
docker build -t "${IMAGE_BACKEND}:latest" -f "${ROOT_DIR}/Dockerfile" "${ROOT_DIR}" \
  || fail "backend image build failed"

# ---------- 导出镜像为 tar ----------
log "saving images to ${LOCAL_TAR}"
docker save -o "${LOCAL_TAR}" "${IMAGE_FRONTEND}:latest" "${IMAGE_BACKEND}:latest" \
  || fail "docker save failed"

# ---------- 同步配置文件到远端 ----------
SYNC_FILES=(
  docker-compose.yml
  .env
)

log "ensuring remote directory exists: ${REMOTE}:${DIR}"
ssh "${SSH_OPTS[@]}" "${REMOTE}" "mkdir -p '${DIR}'" \
  || fail "failed to create remote directory ${DIR}"

log "syncing config files to remote host"
for f in "${SYNC_FILES[@]}"; do
  if [ -f "${ROOT_DIR}/${f}" ]; then
    rsync -az -e "ssh -p ${PORT} -o BatchMode=yes -o StrictHostKeyChecking=accept-new" \
      "${ROOT_DIR}/${f}" "${REMOTE}:${DIR}/${f}" || fail "rsync ${f} failed"
  fi
done

# ---------- 上传镜像 tar ----------
log "uploading image tar to remote host"
rsync -az --progress -e "ssh -p ${PORT} -o BatchMode=yes -o StrictHostKeyChecking=accept-new" \
  "${LOCAL_TAR}" "${REMOTE}:${DIR}/${TAR_FILE}" \
  || fail "image upload failed"

# ---------- 远端：删除旧镜像、加载新镜像、重启容器 ----------
run_remote "stopping containers" "docker compose down || true"

run_remote "removing old images" \
  "docker rmi ${IMAGE_FRONTEND}:latest ${IMAGE_BACKEND}:latest 2>/dev/null || true"

run_remote "loading new images" "docker load -i '${TAR_FILE}'"

run_remote "starting containers" "docker compose up -d"

run_remote "cleaning up image tar" "rm -f '${TAR_FILE}'"

run_remote "showing container status" "docker compose ps"

# ---------- 清理本地临时文件 ----------
log "cleaning up local tar"
rm -f "${LOCAL_TAR}"

# ---------- 远端健康检查 ----------
log "waiting for remote services to become healthy"
ssh "${SSH_OPTS[@]}" "${REMOTE}" 'bash -s' -- "${DIR}" <<'EOF' || fail "remote health check failed"
set -euo pipefail

DIR="$1"
cd "$DIR"

printf '[remote] waiting for service health checks\n'

services=$(docker compose config --services)
deadline=$((SECONDS + 90))

for service in $services; do
  printf '[remote] checking service: %s\n' "$service"

  while :; do
    container_id=$(docker compose ps -q "$service")

    if [ -z "$container_id" ]; then
      printf '[remote] ERROR: service %s has no container id\n' "$service" >&2
      exit 1
    fi

    state=$(docker inspect --format '{{.State.Status}}' "$container_id")
    health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container_id")

    printf '[remote] service=%s state=%s health=%s\n' "$service" "$state" "$health"

    if [ "$state" != "running" ]; then
      printf '[remote] ERROR: service %s is not running\n' "$service" >&2
      docker compose ps "$service" >&2 || true
      exit 1
    fi

    if [ "$health" = "healthy" ] || [ "$health" = "none" ]; then
      break
    fi

    if [ "$health" = "unhealthy" ] || [ $SECONDS -ge $deadline ]; then
      printf '[remote] ERROR: service %s failed health check\n' "$service" >&2
      docker compose ps "$service" >&2 || true
      docker compose logs --tail=100 "$service" >&2 || true
      exit 1
    fi

    sleep 5
  done
done
EOF

log "deployment completed successfully on ${REMOTE}:${DIR}"
