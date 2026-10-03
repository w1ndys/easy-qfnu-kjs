# 查询契约 v2

全部只读 GET。统一响应四段式：`{code, message, data, request_id}`。

- `code`：数字，`0` 成功，非 0 错误。
- `message`：人类可读说明。
- `data`：业务载荷（见各端点）。
- `request_id`：服务端为每次请求生成的追踪号，用于日志定位。

## 端点

| 路径 | 参数 | `data` |
|---|---|---|
| `GET /api/context` | — | `{date, term, week, weekday, in_calendar}` |
| `GET /api/meta` | — | 数据版本、字典、轴、可用学期与逐周新鲜度 |
| `GET /api/query` | `view`、`keyword`、`date_offset`、`start_node`、`end_node`、`limit`、`offset` | `{total, items, date, week, day_of_week}` |

## code

| `code` | 含义 |
|---|---|
| `0` | 成功 |
| `40001` | 参数错误（非法 `view`、`start_node > end_node`、`date_offset` 越界、`keyword` 过长等） |
| `40401` | 无数据（没有任何可用 release，或该日期无已发布数据） |
| `50000` | 内部错误 |

## 参数

- `view`：`availability`（空教室）或 `day`（状态列表）。
- `keyword`：教室名子串匹配；NFKC + 去首尾空白，ASCII 不区分大小写，最长 32，不支持正则；省略时返回全库。
- `date_offset`：`0..10`，默认 `0`（今天）；服务端按 `Asia/Shanghai` 与学期日历解析成 `(term, week, weekday, date)`。
- `start_node` / `end_node`：仅 `availability`；两位节次 `01`–`12`，默认 `01`、`11`，要求 `start_node ≤ end_node`。
- `limit` / `offset`：分页，默认 `50` / `0`。

## 空教室（view=availability）

给定 `date_offset` 解析出的星期内，名称匹配的每个房间，`start_node` 到 `end_node` 的**每一节**都可用才返回。可用 = 字典 `available=true` 的 `free` 与 `fully_free`。

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "total": 12,
    "items": [ { "id": "0899", "name": "F126" } ],
    "date": "2025-09-15", "week": 2, "day_of_week": 1
  },
  "request_id": "req_abc123"
}
```

## 状态列表（view=day）

同一筛选，返回每个房间该日的 12 小节状态（只按 12 小节，无折叠）。

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "total": 12,
    "items": [
      { "id": "0899", "name": "F126",
        "statuses": { "01": "free", "02": "free", "03": "class", "04": "class", "05": "class",
                      "06": "free", "07": "free", "08": "free", "09": "free",
                      "10": "free", "11": "free", "12": "free" } }
    ],
    "date": "2025-09-15", "week": 2, "day_of_week": 1
  },
  "request_id": "req_abc123"
}
```

## meta 的 data

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "release_id": "20261003T041012+0800",
    "generated_at": "2025-09-15T04:10:12+08:00",
    "dict_version": 1,
    "axis_version": 1,
    "states": {
      "free":       { "label": "空闲",     "available": true },
      "fully_free": { "label": "完全空闲", "available": true },
      "class":      { "label": "正常上课", "available": false }
    },
    "axis": {
      "blocks": ["0102", "030405", "0607", "0809", "101112"],
      "nodes":  [ { "code": "01", "block": "0102" } ]
    },
    "terms": [
      { "term": "2025-2026-3", "total_weeks": 20,
        "weeks": [
          { "week": 2, "monday": "2025-09-15", "published": true,
            "last_success_at": "2025-09-15T04:10:12+08:00", "stale": false }
        ] }
    ]
  },
  "request_id": "req_abc123"
}
```

## 不在教学周

正常返回，由 `/api/context` 的 `in_calendar=false` 表达；前端提示后仍允许查询（照现状）。

## 新鲜度

新鲜度由查询侧计算并落在 `/api/meta` 的逐周信息里：当前周与未来 4 周距最近成功超过 36 小时为过期，其余周超过 8 天为过期；最近一次采集失败也算过期。过期仍返回上一版成功数据并标记 `stale: true`。

## 错误示例

```json
{ "code": 40401, "message": "no published data", "data": null, "request_id": "req_abc123" }
```
