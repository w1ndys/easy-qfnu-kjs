# 教务系统请求与响应结构（脱敏参考）

- 日期：2026-10-23
- 来源：教师账号只读探测、旧实现记录、当前代码对照
- 用途：后续重写、排障、写解析器时查请求参数与响应结构
- 边界：不保存账号、密码、Cookie、Token、验证码、原始完整页面；示例中的 id、会话、楼号、房号均为占位符

> 产品口径以 [docs/prd.md](prd.md) 为准；本文只描述上游 HTTP 结构。
> 旧 [docs/upstream.md](upstream.md) 的 CAS 描述已被实测推翻，保留作历史。

## 1. 通用约定

- 根地址：`http://zhjw.qfnu.edu.cn`，可由 `UPSTREAM_BASE_URL` 覆盖。
- 编码：页面正文 UTF-8；表单 `application/x-www-form-urlencoded`；明细用 GET 查询串。
- 会话：登录后带 `Cookie: JSESSIONID=<会话>`；会话失效时业务页会变成登录页或“非法访问”。
- 客户端请求头（实测可用）：
  - `User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36`
  - POST 带 `Content-Type: application/x-www-form-urlencoded`
  - GET 不需要额外头；带同一个 Cookie
- 响应状态：正常为 HTTP 200；非 200 按失败处理。
- 200 也可能是登录页：正文含 `用户登录` 或 `非法访问` 时按会话失效处理，不要当数据。
- 请求节奏：相邻请求至少 500ms（每秒至多 2 次）；单次最多 3 次重试；明细必须可断点。
- **单会话**：同一教师账号在别处登录会让当前会话失效；采集器只保持一个会话，排障脚本不要和采集同时登录。

## 2. 登录

### 2.1 打开登录页

```http
GET / HTTP/1.1
Host: zhjw.qfnu.edu.cn
User-Agent: Mozilla/5.0 ...
```

响应（200，HTML，约 9KB）：

- 标题：`登录`
- Set-Cookie：`JSESSIONID=<会话>; Path=/; HttpOnly`
- 关键结构：

```html
<form id="loginForm" name="loginForm" action="/Logon.do?method=logonLdap" method="post">
  <input id="userAccount" name="userAccount" type="text">
  <input id="userPassword" name="userPassword" type="password">
  <input id="RANDOMCODE" name="RANDOMCODE" type="text">
  <input id="encoded" name="encoded" type="hidden">
  <img id="SafeCodeImg" src="/verifycode.servlet" width="80px" height="40px">
</form>
```

### 2.2 验证码

```http
GET /verifycode.servlet HTTP/1.1
Cookie: JSESSIONID=<会话>
```

响应：

- 200
- **JPEG**，80×40（不是 PNG；扩展名不能按 `.png` 处理）
- 必须与后续登录请求共用同一个 Cookie 会话
- 识别结果作为 `RANDOMCODE`；OCR 服务异常也可能返回 HTTP 200，以业务 `code=200` 判成功

### 2.3 取登录参数

```http
POST /Logon.do?method=logon&flag=sess HTTP/1.1
Content-Type: application/x-www-form-urlencoded
Cookie: JSESSIONID=<会话>

（空表单）
```

响应正文：

```text
<scode>#<sxh>
```

- `scode` 是插入用的字符池，`sxh` 是每位要插入的长度数字串。
- 返回 `no` 或格式不对时不能继续。

### 2.4 生成 encoded

```text
code = username + "%%%" + password
encoded = ""
for i in 0..len(code)-1:
    if i < 20:
        encoded += code[i]
        encoded += scode[0 : int(sxh[i])]
        scode = scode[int(sxh[i]) :]
    else:
        encoded += code[i:]
        break
```

- 只对前 20 个字符插 `scode`；剩余原样追加。
- `username`/`password` 的字符按原样处理。

### 2.5 提交登录

```http
POST /Logon.do?method=logonLdap HTTP/1.1
Content-Type: application/x-www-form-urlencoded
Cookie: JSESSIONID=<会话>

userAccount=&userPassword=&RANDOMCODE=<验证码>&encoded=<encoded>
```

- `userAccount`、`userPassword` 留空；凭据在 `encoded` 里。
- 响应 200，正文可能是跳转脚本或登录结果页。
- 成功标记：`正在登录`、`location`、`教学一体化服务平台`；有 `location` 时跟随。
- 密码错误标记：`密码错误`、`用户名或密码错误`、`您提供的用户名或者密码有误` → 立即失败，不重试。
- 验证码错误标记：`验证码错误`、`验证码不正确` → 换新验证码重试，最多 3 次。

### 2.6 验证登录

```http
GET /jsxsd/framework/jsMain.jsp HTTP/1.1
Cookie: JSESSIONID=<会话>
```

- 200 且正文含 `教学一体化服务平台` 才算登录完成。
- 否则按失败处理，不要拿空会话去请求业务页。

## 3. 学期页

```http
GET /jsxsd/kbxx/jsjy_query HTTP/1.1
Cookie: JSESSIONID=<会话>
```

响应（200，HTML，约 43KB）：

- 正文含学期编号 `YYYY-YYYY-N`，例如 `2026-2027-1`。
- 含节次模式：

```html
<input type="hidden" id="kbjcmsid" name="kbjcmsid" value="<节次模式 id>">
```

- `kbjcmsid` 是明细请求的必填参数；每次登录/学期可能不同，不要写死。
- 教学楼下拉 `select#jxlbh` 初始只有“请选择”，楼列表通过 AJAX 填充。

## 4. 周历页

```http
POST /jsxsd/framework/main_index_loadkb.jsp HTTP/1.1
Content-Type: application/x-www-form-urlencoded
Cookie: JSESSIONID=<会话>

rq=YYYY-MM-DD
```

- `rq` 取 `Asia/Shanghai` 当天日期。
- 响应（200，HTML，约 5KB）正文含：

```text
第5周 /16周
```

- 解析式建议：`第\s*(\d+)\s*周\s*/\s*共?\s*(\d+)\s*周`，兼容“共”和空白。
- 同一句在 `GET /jsxsd/framework/jsMain_new.jsp?t1=1` 里也有；采集固定用上面的 POST。
- 非教学周可能没有这句；取不到不要猜周次。

## 5. 教学楼列表（AJAX）

```http
GET /jsxsd/kbxx/jsjy_processAjax?xqid=1&requestType=jxl HTTP/1.1
Cookie: JSESSIONID=<会话>
```

参数：

- `xqid`：校区 id，`1` 曲阜、`2` 日照（实测）。
- `requestType`：`jxl` 教学楼下拉；`jxq` 教学区；`newjs` 教室；`gnq` 功能区。

响应（200，JSON）：

```json
[
  { "dm": "B001", "dmmc": "1号教学楼" },
  { "dm": "B002", "dmmc": "2号教学楼" }
]
```

- `dm` 就是矩阵/白名单用的 `jxlbh`；`dmmc` 是展示名。
- 白名单只存 `jxlbh` 与展示名；采集不把楼名写进 `jsmc_mh`。
- 实测曲阜 26 栋、日照 11 栋（2026-10-23）。

## 6. 周矩阵

```http
POST /jsxsd/kbxx/jsjy_query2 HTTP/1.1
Content-Type: application/x-www-form-urlencoded
Cookie: JSESSIONID=<会话>

typewhere=jszq
xnxqh=YYYY-YYYY-N
jxlbh=<楼编号>
jsmc_mh=
zc=<周次>
zc2=<周次>
xq=1
xq2=7
```

- `jc`、`jc2`、`jszt` 不传；`jsmc_mh` 留空；`jxlbh` 不得为空。
- 一栋楼一周一次请求。
- 响应（200，HTML，约 170KB/30 间房）包含两张表：

```html
<table id="dataListHeader" ...> ... </table>
<table id="dataList" name="kbDataTable" ...>
  <tr height="60">
    <td></td>
    <td tdvalue="0102">01<br>02</td>
    <td tdvalue="030405">03<br>04<br>05</td>
    ...
  </tr>
  <tr jsbh="R0001" onclick="...">
    <td><input type="checkbox" value="R0001">1号教学楼101(90/0)</td>
    <td><font color='black'>◆</font></td>
    <td></td>
    ...
  </tr>
</table>
```

结构要点：

- `dataListHeader` 在 `dataList` 之前；按 id 精确选 `dataList`，不要用前缀匹配。
- 表头行 36 个 `td`，其中 35 个带 `tdvalue`；第一个房间列不带。
- 35 个块格按天重复 5 大节：
  - `0102` → 01、02
  - `030405` → 03、04、05
  - `0607` → 06、07
  - `0809` → 08、09
  - `101112` → 10、11、12
- 数据行 36 个 `td`：1 个房间列 + 35 个状态格。
- 房间身份优先 `tr[jsbh]`；没有时用行内 `input[type=checkbox]` 的 `value`；再退回首列第一个 `input[value]`。
- 房名带容量片段，例如 `1号教学楼101(90/0)`；规范化时删容量片段，保留方位/校区括号。
- 状态格：

| 原文 | 语义 |
|---|---|
| 空 | 空闲 |
| `空闲` | 空闲 |
| `◆` | 正常上课 |
| `Ｊ` | 借用（全角 J） |
| `Ｘ` | 锁定（全角 X） |
| `Κ` | 考试（希腊 Kappa） |
| `Ｇ` | 固定调课（全角 G） |
| `Ｌ` | 临时调课（全角 L） |
| `M` | 跨模式占用（ASCII） |
| 多个已知符号 | 复合占用，例如 `◆Ｊ` |
| 其它文本 | 未知，保留原文 |

- “完全空闲”不会出现在格子里；空白格就是空闲。

## 7. 完全空闲筛选（房间列表模式）

```http
POST /jsxsd/kbxx/jsjy_query2 HTTP/1.1
Content-Type: application/x-www-form-urlencoded

typewhere=jszq&xnxqh=YYYY-YYYY-N&jxlbh=<楼编号>&jsmc_mh=&bjfh==
&jszt=8&zc=<周次>&zc2=<周次>&xq=1&xq2=1&jc=01&jc2=02
```

- `jszt=8` 是“完全空闲”筛选：返回所选日期与节次范围内整间教室全空的房间列表。
- 响应是**房间列表**，不是矩阵；`table#dataList` 每行 2 列（房间、状态），没有 35 格。
- 实测：Q10 周一 01–02 节返回 17 间；整周 `xq=1..7` 时 Q10 返回 0 间、Q03 返回 49 间。
- 这是房间级、按范围成立的结论，不是格子状态；本地查询用区间内每节 `available` 聚合表达。

## 8. 占用明细

```http
GET /jsxsd/kbxx/jsjy_jszyqk?xnxqh=YYYY-YYYY-N&jsbh=R0001&kcsj=10102&xq=1&typewhere=jszq&startZc=&endZc=&startJc=&endJc=&startXq=1&endXq=7&kssj=&jssj=&jszt=&type=add&kbjcmsid=<节次模式 id> HTTP/1.1
Cookie: JSESSIONID=<会话>
```

- `kcsj=<星期><大节>`：周一 0102 就是 `10102`。
- `xq` 与 `kcsj` 第一位一致。
- `kbjcmsid` 来自学期页。
- 响应（200，HTML，约 3–9KB）分两部分：

```html
<center id="alldiv">
  <table id="tab1">
    <tr><td class="toolstitle">教室占用情况</td></tr>
  </table>
</center>

<form action="/jsxsd/jspyfa/selectJx02" name="Form1" id="Form1" method="post">
  <table border="1">
    <tr><td>
      <table border="0">
        <tr>
          <td width="173" align="right" bgcolor="#EFEFEF">教室状态：</td>
          <td width="9"></td>
          <td width="560">正常上课</td>
        </tr>
        <tr>
          <td align="right" bgcolor="#EFEFEF">教室：</td>
          <td></td>
          <td>1号教学楼101</td>
        </tr>
        <tr>
          <td align="right" bgcolor="#EFEFEF">周次：</td>
          <td></td>
          <td>1-16</td>
        </tr>
        <tr>
          <td align="right" bgcolor="#EFEFEF">时间标志：</td>
          <td></td>
          <td>单双周</td>
        </tr>
        <tr>
          <td align="right" bgcolor="#EFEFEF">课程：</td>
          <td></td>
          <td>某课程</td>
        </tr>
        <!-- 备注、申请人、任课教师等也在这里，解析时丢弃 -->
      </table>
    </td></tr>
  </table>
</form>
```

结构要点：

- **`table#tab1` 只是标题表**，里面没有字段；字段在后面的嵌套表格里。
- 字段行固定三列“标签 / 空列 / 值”，标签带全角冒号（`教室状态：`、`课程：`、`周次：`、`时间标志：`）。
- 按整页 `tr` 扫描、只认三列且标签去掉冒号后属于四个字段的行，不要按 `table#tab1` 找字段。
- 只保留 `教室状态`、`课程`、`周次`、`时间标志`；申请人、任课教师、备注、教室、时间、节次一律丢弃。
- 同一格可能有多条记录；以 `教室状态` 开头分条，按行顺序保留。
- 空格子/无占用时，响应仍可能有 `table#tab1` 标题，但没有字段行；返回空列表。
- 若连 `table#tab1` 都没有，说明不是明细页，按结构失败处理。
- 明细的周次范围可能覆盖过去/未来（如 `1-16`）；采集只保留与采集窗口相交的部分。

## 9. 关键坑点

- **不要用正则抠 HTML 结构**：`dataListHeader` 在 `dataList` 前，前缀匹配会选错表；明细字段也不在 `tab1` 里。用 goquery/CSS 选择器。
- **验证码是 JPEG**，不是 PNG；`/verifycode.servlet` 要与登录同一 Cookie 会话。
- **登录不是 CAS**：强智直登 `/Logon.do?method=logonLdap`；旧文档的 CAS 描述作废。
- **单会话互踢**：同一账号别处登录会让本会话失效；采集与排障脚本不要并行登录。
- **`kbjcmsid` 动态**：从学期页取，不能写死。
- **`jsbh` 是唯一身份**：房名可重复、带容量；不要用房名做主键。
- **`完全空闲` 不是格子文本**：格子空白就是空闲；`jszt=8` 是房间级筛选。
- **明细标签带冒号**：比较前去掉空白与末尾 `：`/`:`。
- **200 不等于数据页**：正文含 `用户登录`/`非法访问` 时按会话失效。
- **请求节奏**：500ms/次，最多 3 次重试；明细量大，必须断点续传。

## 10. 探测记录（脱敏）

- 学期：`2026-2027-1`，第 5 周 / 共 16 周；曲阜 26 栋、日照 11 栋。
- 登录页约 9KB；验证码 80×40 JPEG；学期页约 43KB；周历页约 5KB。
- Q10 第 5 周矩阵约 169KB、30 行 × 35 格；`◆` 345、`Ｊ` 114、空白 591。
- 26 栋全学期聚合矩阵：空白 36,474、`◆` 2,706、`Ｊ` 974，复合 50+；“完全空闲”0。
- 明细：有课格约 6KB，空格约 3KB；实测解析出 `教室状态=正常上课、课程=某课程、周次=1-16、时间标志=单双周`。
- 旧版 2025-2026-3 四周快照：状态 8（完全空闲）出现 0 次。

> 原始响应含教师姓名、课程、教室安排等，未入库；本文只保留脱敏结构与少量片段。
