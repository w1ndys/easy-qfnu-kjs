# 查询契约 v2

全部 GET、全部只读。错误一律 `application/problem+json`（RFC 9457），并带稳定扩展字段 `code`。

**本版重定了路径与载荷**：路径按资源命名重新表述，载荷改用语义状态键与节点区间，不再保留"旧词汇的翻译层"。

## 约定

- 每个成功响应都带 `schema_version`（契约版本）与 `data`（数据版本：`release_id`、`dict_version`、逐周新鲜度）。
- 路径不带版本段：当前没有已部署的旧客户端，路径版本只会造出"两个版本并存"的错觉；数据版本用 `release_id` 表达。
- 时间一律 `Asia/Shanghai`，由服务端时钟决定；客户端不参与推算。

## 资源

| 路径 | 作用 |
|---|---|
| `GET /api/context` | 当前时间上下文：日期、学期、周次、星期、是否在教学周 |
| `GET /api/meta` | 数据版本与字典：`release_id`、生成时间、`dict_version`、`axis_version`、状态字典、节次轴 |
| `GET /api/terms` | 学期与周：总周数、每周一日期、是否在日历内、该周是否有数据与新鲜度 |
| `GET /api/groups` | 查询视图：`id`、`name`、`order`、`room_count` |
| `GET /api/availability` | 空教室：给定视图与节点区间，返回区间内每一节都可用的房间 |
| `GET /api/day` | 全天状态：同一筛选，返回每个房间该日的节次状态 |

## 参数

公共：

- `group`：可重复，多视图取并集；省略表示全部视图。
- `q`：关键词，仅在该视图已发布的房名上做子串匹配。先去首尾空白并做 NFKC，ASCII 字母不区分大小写，最长 32，不支持正则。
- `term`、`week`、`weekday`（1 = 周一；省略时用 `/api/context` 的当前值）。

`/api/availability` 另有：

- `from`、`to`：两位节点码（`01`—`12`），默认 `01` 与 `12`；要求 `from ≤ to`。

`/api/day` 另有：

- `fold`：`nodes`（默认，12 小节）或 `blocks`（5 个大节，按 `axis_node.block` 归组）。

## 空教室判定

所选星期内，名称匹配的每个房间，`from` 到 `to` 的**每一节**都 `available` 才返回。
可用只有字典里 `available = true` 的两个状态：`free` 与 `fully_free`。`unknown` 与 `composite` 一律不可用。

## 载荷

`GET /api/context`

```json
{ "schema_version": 2,
  "date": "2025-09-15", "term": "2025-2026-3",
  "week": 2, "weekday": 1, "in_calendar": true }
```

`GET /api/meta`

```json
{ "schema_version": 2,
  "release_id": "sha256:…", "generated_at": "2025-09-15T04:10:12+08:00",
  "dict_version": 1, "axis_version": 1,
  "states": {
    "free":       { "label": "空闲",     "available": true },
    "fully_free": { "label": "完全空闲", "available": true },
    "class":      { "label": "正常上课", "available": false }
  },
  "axis": {
    "blocks": ["0102", "030405", "0607", "0809", "101112"],
    "nodes":  [ { "code": "01", "block": "0102" } ]
  } }
```

`GET /api/terms`

```json
{ "schema_version": 2,
  "terms": [
    { "term": "2025-2026-3", "total_weeks": 20,
      "weeks": [
        { "week": 2, "monday": "2025-09-15", "in_calendar": true,
          "published": true, "last_success_at": "2025-09-15T04:10:12+08:00", "stale": false }
      ] } ] }
```

`GET /api/groups`

```json
{ "schema_version": 2,
  "groups": [ { "id": "ja", "name": "JA", "order": 4, "room_count": 120 } ] }
```

`GET /api/availability?group=ja&q=F1&week=2&weekday=1&from=01&to=02`

```json
{ "schema_version": 2,
  "query": { "group": ["ja"], "q": "F1", "term": "2025-2026-3",
             "week": 2, "weekday": 1, "from": "01", "to": "02" },
  "count": 12,
  "rooms": [ { "id": "0899", "name": "F126", "capacity": 90 } ],
  "data": { "release_id": "sha256:…", "dict_version": 1,
            "last_success_at": "2025-09-15T04:10:12+08:00", "stale": false } }
```

`GET /api/day?group=ja&week=2&weekday=1&fold=nodes`

```json
{ "schema_version": 2,
  "query": { "group": ["ja"], "term": "2025-2026-3", "week": 2, "weekday": 1 },
  "fold": "nodes",
  "rooms": [
    { "id": "0899", "name": "F126",
      "states": { "01": "free", "02": "free", "03": "class", "04": "class", "05": "class",
                  "06": "free", "07": "free", "08": "free", "09": "free",
                  "10": "free", "11": "free", "12": "free" } }
  ],
  "data": { "release_id": "sha256:…", "dict_version": 1, "stale": false } }
```

`fold=blocks` 时 `states` 的键变成块编码（`0102`、`030405`、…），值不变。

## 错误码（稳定扩展字段 `code`）

| `code` | HTTP | 场景 |
|---|---|---|
| `invalid_parameter` | 400 | 参数格式或范围错误（含 `from > to`、未知 `fold`、关键词过长） |
| `unknown_group` | 404 | 视图不在当前 release |
| `not_published` | 404 | 该学期或该周没有可用数据 |
| `no_data` | 503 | 完全没有任何可用 release |
| `not_found` | 404 | 未知路径 |
| `method_not_allowed` | 405 | 非 GET |
| `internal` | 500 | 未预期错误 |

## 新鲜度与缓存

- 新鲜度由查询侧计算，不写死在库里：当前周与未来 4 周距最近成功超过 36 小时为过期，其余周超过 8 天为过期；最近一次采集失败也算过期。过期仍返回上一版成功数据，并带上时间与 `stale: true`。
- 缓存：`/api/meta`、`/api/terms`、`/api/groups` 可长缓存（随 `release_id` 变化失效）；`/api/context` 短缓存（分钟级）；两个查询端点按 `release_id` 缓存。

## 与 v1 的路径对照

| v1 | v2 | 变化 |
|---|---|---|
| `GET /api/v1/manifest` | `GET /api/meta` | 去掉"清单"这个混合概念：数据版本归 `meta`，时间上下文归 `context` |
| `GET /api/v1/context` | `GET /api/context` | 载荷字段与 v1 一致 |
| — | `GET /api/terms` | 新增：学期与周（含每周日期与新鲜度），v1 只有 manifest 里的部分字段 |
| — | `GET /api/groups` | 新增：视图列表独立成资源 |
| `GET /api/v1/empty-classrooms` | `GET /api/availability` | 名称改为资源语义；参数 `start_node`/`end_node` → `from`/`to`；`group_id` → 可重复的 `group` |
| `GET /api/v1/full-day-status` | `GET /api/day` | 名称与参数同上，另加 `fold=nodes|blocks` |
