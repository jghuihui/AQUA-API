#!/bin/sh
# 安全部署脚本：data/ 目录是唯一的数据来源，这个脚本的核心职责是【不动它】。
#
# 【本脚本存在的原因：一次真实事故】
#
#   2026-10-03 更新版本时执行了
#       rm -rf /opt/aqua-api && mkdir -p /opt/aqua-api && tar xzf ... -C /opt/aqua-api
#   而数据库就在 /opt/aqua-api/data/aqua.db（compose 里挂 ./data:/data）。
#   结果整个站点数据被删 —— 渠道、用户、agent 配置全没。
#
#   事故本身有两个可复盘的点，脚本把它们都固化成了硬约束：
#
#   1) "整目录换掉以避免 tar 残留旧文件"是对的，但【必须把 data/ 移出删除范围】。
#      tar 确实不会删除源端已不存在的文件，可那个前提是"目录还在"。
#      正确顺序：先备份 data/ → 换代码目录 → 原样搬回 data/ → 才 docker compose up。
#
#   2) 删之前必须 docker inspect 查挂载点。
#      凭记忆猜"data 在哪"就是这次出事的直接原因。
#
# 用法：
#   sh deploy-safe.sh <镜像标签>     例：sh deploy-safe.sh local-v7
set -e

TAG="${1:?用法: sh deploy-safe.sh <镜像标签>，例 local-v7}"
APP_DIR="${APP_DIR:-/opt/aqua-api}"
DATA_DIR="$APP_DIR/data"
BACKUP_ROOT="${BACKUP_ROOT:-/root}"
STAMP="$(date +%Y%m%d-%H%M%S)"
BACKUP_DIR="$BACKUP_ROOT/aqua-db-$STAMP"

echo "==> 目标目录: $APP_DIR"
echo "==> 镜像标签: aqua-api:$TAG"

# ── 第 1 步：先摸清数据在哪，不猜 ──────────────────────────────
#
# 【为什么要从运行中的容器反查，而不是直接用 $DATA_DIR】
#   万一挂载点在别处（换过部署路径、用了具名卷），
#   我们删掉的那个"看起来是代码目录"的东西其实正挂着数据库。
#   docker inspect 是唯一可靠的来源。
MOUNT_SRC="$(docker inspect aqua-api --format \
  '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Source}}{{end}}{{end}}' 2>/dev/null || true)"

if [ -n "$MOUNT_SRC" ]; then
  DATA_DIR="$MOUNT_SRC"
  echo "==> 数据目录（由容器挂载反查）: $DATA_DIR"
else
  echo "==> 警告：容器未运行或无 /data 挂载，回退到 $DATA_DIR" >&2
  echo "==> 请人工确认该路径【不含】数据库后再继续" >&2
  exit 1
fi

# ── 第 2 步：备份并验证备份 ────────────────────────────────────
#
# 【为什么要单独验一遍完整性】
#   "cp 成功"不等于"拷到的是完整库"。曾经真的发生过：
#   备份文件 integrity_check 是 ok，但 WAL 没被一起带走，
#   打开后渠道数为 0 —— 看起来有备份，实际是空的。
#   所以这里做 checkpoint 后再查关键表，而不是只查 integrity_check。
mkdir -p "$BACKUP_DIR"
cp "$DATA_DIR"/aqua.db* "$BACKUP_DIR"/ 2>/dev/null || true
chmod u+w "$BACKUP_DIR"/* 2>/dev/null || true

# 【为什么用绝对路径，而不是 ( cd "$BACKUP_DIR" ; ... ) 包成一个子 shell】
#
#   这里原本写的是子 shell。三个变量在子 shell 里赋值、在父 shell 里读，
#   读到的永远是空串，于是 `${INTEGRITY:-失败}` 恒为"失败"——
#   脚本每一次都停在下面那个"备份校验不通过"，包括本该放行的部署。
#   它看起来像一道闸门，实际是把所有情况都判成危险，因此从来没跑完过。
#
#   不需要 cd：sqlite3 会把 -wal/-shm 建在库文件旁边，传绝对路径即可。
BK_DB="$BACKUP_DIR/aqua.db"
if [ ! -f "$BK_DB" ]; then
  echo "==> 备份文件不存在: $BK_DB（复制阶段就失败了）" >&2
  exit 1
fi
# WAL 里可能有未落盘的数据。不合并就等于只备份了一部分。
sqlite3 "$BK_DB" 'PRAGMA wal_checkpoint(TRUNCATE);' >/dev/null 2>&1 || true
# head -1：integrity_check 正常时只输出一行 "ok"，异常时会输出多行，取首行便于比对。
INTEGRITY="$(sqlite3 "$BK_DB" 'PRAGMA integrity_check;' 2>/dev/null | head -1)"
CHANNELS="$(sqlite3 "$BK_DB" 'SELECT count(*) FROM channels;' 2>/dev/null || echo '?')"
USERS="$(sqlite3 "$BK_DB" 'SELECT count(*) FROM users;' 2>/dev/null || echo '?')"
echo "==> 备份: $BACKUP_DIR（完整性=${INTEGRITY:-失败} 渠道=$CHANNELS 用户=$USERS）"

if [ "$INTEGRITY" != "ok" ]; then
  echo "==> 备份校验不通过，中止部署" >&2
  exit 1
fi
if [ "$CHANNELS" = "0" ] && [ "$USERS" = "0" ]; then
  # 不是"一定错"（全新站点确实是 0），但空站是极罕见的，
  # 而本站有渠道与用户 —— 在这里停下来问一句比事后才发现好。
  echo "==> 备份里渠道与用户都是 0。全新站点可忽略，否则输入 yes 继续：" >&2
  read -r ans
  [ "$ans" = "yes" ] || exit 1
fi

# ── 第 3 步：从容器反查并重建 compose ──────────────────────────
#
# 【为什么不用磁盘上已有的 docker-compose.yml】
#   事故当晚 /opt/aqua-api 里的 compose 写的是 container_name: ltzy-api，
#   而实际跑的是 aqua-api —— 磁盘上那份是过时且不可信的。
#   容器当前的 inspect 结果是【正在生效】的配置，比任何文件都权威。
#   唯一要改的是镜像标签。
docker inspect aqua-api --format '{{range .Config.Env}}{{println .}}{{end}}' \
  2>/dev/null | grep -v '^PATH=' > "$BACKUP_DIR/env.txt" || true
docker inspect aqua-api --format '{{index .Config.Labels "com.docker.compose.project"}}' \
  2>/dev/null > "$BACKUP_DIR/project.txt" || true
PROJECT="$(cat "$BACKUP_DIR/project.txt" 2>/dev/null || echo aqua-api)"
SERVICE="$(docker inspect aqua-api --format \
  '{{index .Config.Labels "com.docker.compose.service"}}' 2>/dev/null || echo aqua)"

# 记录当前镜像，回滚时要用
CUR_IMAGE="$(docker inspect aqua-api --format '{{.Config.Image}}' 2>/dev/null || echo '')"
echo "$CUR_IMAGE" > "$BACKUP_DIR/previous-image.txt"
if [ -n "$CUR_IMAGE" ] && [ "$CUR_IMAGE" != "aqua-api:$TAG" ]; then
  docker tag "$CUR_IMAGE" "aqua-api:rollback-$STAMP" 2>/dev/null || true
  echo "==> 回滚镜像: aqua-api:rollback-$STAMP（原 $CUR_IMAGE）"
fi

# ── 第 4 步：换代码（data/ 绝不进删除范围）─────────────────────
#
# 关键：把 data/ 挪到 APP_DIR 之外，再重建目录。
# mv 同一文件系统是瞬时的，比 cp 快也比分两步安全。
# 数据暂存与代码归档都放在 APP_DIR 的【父目录】下，
# 且各用独立前缀：暂存的是数据，归档的是代码，
# 两者若落在同一目录，事后翻找时很容易把备份当成代码、或反过来。
# 独立前缀也让"这一步动过什么"在 ls 一眼里就能看清。
STASH="$(dirname "$APP_DIR")/.aqua-data-stash-$STAMP"
CODE_OLD="$(dirname "$APP_DIR")/.aqua-code-old-$STAMP"

echo "==> 暂存数据目录: $DATA_DIR -> $STASH"
mv "$DATA_DIR" "$STASH"

# 归档旧代码（不直接删：出问题时还能翻）。
# 顺序上必须在数据暂存【之后】——否则 mv 会把 data/ 一起带走，
# 归档里就多出一份数据，而真正放回去时目标目录已被占用。
# 这是本脚本最容易写错的一步：两个 mv 都在动同一个目录树。
echo "==> 归档旧代码: $APP_DIR -> $CODE_OLD"
rm -rf "$CODE_OLD" 2>/dev/null || true
mv "$APP_DIR" "$CODE_OLD"
mkdir -p "$APP_DIR"
tar xzf "${TARBALL:-/tmp/aqua-src.tar.gz}" -C "$APP_DIR" --strip-components=1

echo "==> 放回数据目录"
mv "$STASH" "$DATA_DIR"

# 数据必须在位才能继续 —— 这是不可逆操作前最后一道闸
if [ ! -f "$DATA_DIR/aqua.db" ]; then
  echo "==> 数据文件缺失，中止。备份在 $BACKUP_DIR" >&2
  exit 1
fi
echo "==> 数据已就位: $DATA_DIR/aqua.db"

# ── 第 5 步：生成 compose 并切容器 ─────────────────────────────
python3 - "$BACKUP_DIR/env.txt" "$APP_DIR/docker-compose.yml" "$TAG" << 'PY'
import sys
env_path, out_path, tag = sys.argv[1], sys.argv[2], sys.argv[3]
envs = []
for line in open(env_path):
    line = line.strip()
    if line and '=' in line:
        k, v = line.split('=', 1)
        envs.append((k, v))
lines = [
    'services:',
    '  aqua:',
    '    image: aqua-api:%s' % tag,
    '    container_name: aqua-api',
    '    restart: unless-stopped',
    '    ports:',
    '      - "8788:8787"',
    '    environment:',
]
# 用长语法（KEY: "VALUE"）而不是短语法（KEY=VALUE）：
# 短语法在这个 compose 版本下会把整块 environment 判成
# "must be a mapping"，而值里含引号时还会被 YAML 二次解析。
for k, v in envs:
    lines.append('      %s: "%s"' % (k, v.replace('"', '\\"')))
lines += [
    '    volumes:',
    '      - ./data:/data',
    '    healthcheck:',
    '      test: ["CMD", "wget", "-qO-", "http://127.0.0.1:8787/healthz"]',
    '      interval: 30s',
    '      timeout: 5s',
    '      retries: 3',
]
open(out_path, 'w').write('\n'.join(lines) + '\n')
PY

cd "$APP_DIR"

# 项目名可能为空：容器若是用 docker run 手工起的就没有 compose 标签。
# 把空串传给 -p 会让 compose 回退到"用目录名当项目名"，那个名字随部署路径变化，
# 于是它既认不出旧容器、又可能算出另一个名字 —— 必须给一个确定值。
# 现网就是这么踩到的：容器无标签 → 项目名落空 → up 与旧容器撞名。
[ -n "$PROJECT" ] || PROJECT=aqua-api
[ -n "$SERVICE" ] || SERVICE=aqua

docker compose -p "$PROJECT" config > /dev/null || {
  echo "==> compose 校验失败，中止。容器未被改动" >&2
  exit 1
}
echo "==> compose 合法，开始切换（项目=$PROJECT 服务=$SERVICE）"

# 【不能吞掉 up 的退出码】
#   这里原本写的是 `up -d 2>&1 | tail -3`。管道让整条命令的退出码变成
#   tail 的（恒为 0），于是 up 失败也继续往下走。
#   实测正是这样：容器名冲突导致 up 失败，脚本一路走到第 6 步，
#   而那时的 healthz 命中的是【一直没停的旧容器】——报出了"部署完成"。
#   先收进变量再打印，退出码才拿得到。
UP_OUT="$(docker compose -p "$PROJECT" up -d 2>&1)"
UP_RC=$?
printf '%s\n' "$UP_OUT" | tail -3

# ── 第 6 步：启动后核对 ────────────────────────────────────────
#
# 【为什么必须先验"跑的是不是新镜像"，而不能只看 healthz】
#   healthz 只证明"8788 上有个健康的东西在答"。旧容器没被换掉时，
#   它答得一样正确 —— 上一步那个假阳性就是这么来的：
#   报"部署完成"的那一刻，接口 uptime 是 3 小时前的。
#   判断"部署成功"的证据只能是【镜像标签对上了】，健康检查是第二步。
sleep 15

RUNNING_IMAGE="$(docker inspect aqua-api --format '{{.Config.Image}}' 2>/dev/null || echo '')"
echo "==> 运行中的镜像: ${RUNNING_IMAGE:-（取不到）}（目标: aqua-api:$TAG）"

if [ "$UP_RC" != "0" ] || [ "$RUNNING_IMAGE" != "aqua-api:$TAG" ]; then
  echo "==> 切换未生效，部署【未】完成" >&2
  echo "==> 若 up 报容器名冲突：旧容器不是本项目的，需先 docker rename 腾出名字" >&2
  echo "==> 数据备份在 $BACKUP_DIR" >&2
  echo "==> 回滚：把 $APP_DIR/docker-compose.yml 里的 image 改回 aqua-api:rollback-$STAMP，再 docker compose -p $PROJECT up -d" >&2
  exit 1
fi

HEALTH="$(curl -s --max-time 10 http://127.0.0.1:8788/healthz || echo 'no-response')"
echo "==> healthz: $HEALTH"
case "$HEALTH" in
  *'"status":"ok"'*) echo "==> 部署完成（镜像已确认为 aqua-api:$TAG）" ;;
  *)
    echo "==> 新镜像已起但健康检查未通过。若因数据问题，备份在 $BACKUP_DIR" >&2
    echo "==> 回滚：把 $APP_DIR/docker-compose.yml 里的 image 改回 aqua-api:rollback-$STAMP，再 docker compose -p $PROJECT up -d" >&2
    exit 1 ;;
esac
