# 后端自动登录改为强智直登流程设计

日期：2026-06-30

## 背景

当前后端通过 `pkg/cas.Client.Login(ctx, username, password)` 执行统一认证 CAS 登录。学校登录入口已经加入滑块验证，导致现有自动登录链路复杂且不稳定。参考 `w1ndys/qfnu-courses-grabber-solo#1` 中“登录流程（SSO 直登）”，本次改为直接登录强智教务系统 `http://zhjw.qfnu.edu.cn`。

验证码 OCR 服务已部署在生产服务器 8000 端口，后端通过环境变量 `OCR_URL` 配置其基础地址，并调用 ddddocr-fastapi 的 `POST /ocr` 接口识别普通图片验证码。

## 目标

- 保持外部调用方式不变：`main.go`、业务查询服务和自动重登录仍使用 `client.Login(ctx, username, password)`。
- 将 `pkg/cas` 内部登录流程替换为强智直登：初始化会话、获取验证码、OCR、获取 `scode/sxh`、生成 `encoded`、提交登录、验证登录状态。
- 最终登录验证页继续使用当前项目已有的 `http://zhjw.qfnu.edu.cn/jsxsd/framework/jsMain.jsp`，避免影响现有空教室查询路径。
- 登录成功/失败继续更新 `pkg/cas/upstream.go` 中的上游健康状态。

## 非目标

- 不保留旧 CAS 登录作为回退路径。旧路径已受滑块影响，回退会增加复杂度且收益有限。
- 不新增前端交互或人工验证码输入流程。
- 不改动空教室查询接口的数据解析逻辑。

## 架构设计

保留 `pkg/cas.Client` 作为登录与会话门面：

- `Client` 继续持有共享 `http.Client`、`CookieJar`、账号密码与重登录锁。
- `Client.Login` 内部切换为强智直登流程。
- `Client.Do` 检测会话失效后仍调用 `retryWithReLogin`，从而复用新登录流程。
- 查询层 `internal/service/classroom.go` 仍只依赖已登录的 `cas.Client`，不感知登录实现变化。

登录实现拆分为小函数，便于测试和维护：

1. 初始化会话：`GET http://zhjw.qfnu.edu.cn`。
2. 获取验证码图片：`GET http://zhjw.qfnu.edu.cn/verifycode.servlet`。
3. OCR 识别：`POST ${OCR_URL}/ocr`。
4. 获取登录加密参数：`POST http://zhjw.qfnu.edu.cn/Logon.do?method=logon&flag=sess`，解析 `scode#sxh`。
5. 生成 `encoded`：按 issue 文档把 `scode` 字符片段插入 `username + "%%%" + password` 的前 20 个字符之间。
6. 提交登录：`POST http://zhjw.qfnu.edu.cn/Logon.do?method=logonLdap`。
7. 验证登录：`GET http://zhjw.qfnu.edu.cn/jsxsd/framework/jsMain.jsp`，检查现有成功标识 `教学一体化服务平台`。

## 登录数据流

`Login(ctx, username, password)` 的流程为：

1. 保存账号密码，供自动重登录使用。
2. 检查 `OCR_URL` 是否已配置；未配置时返回明确错误。
3. 初始化强智教务系统会话。
4. 最多执行 3 次验证码登录尝试：
   - 获取验证码图片字节；图片为空则本次失败。
   - 将图片 base64 编码后调用 OCR：
     - Method: `POST`
     - URL: `${OCR_URL}/ocr`
     - Content-Type: `application/x-www-form-urlencoded`
     - Body: `image=<base64>&probability=false&png_fix=false`
   - 获取 `scode/sxh`。
   - 生成 `encoded`。
   - 提交登录表单：
     - `userAccount=`
     - `userPassword=`
     - `RANDOMCODE=<OCR结果>`
     - `encoded=<编码凭证>`
   - 根据响应判断是否成功、是否验证码错误、是否账号密码错误。
5. 疑似提交成功后访问 `jsxsd/framework/jsMain.jsp` 做最终验证。
6. 验证成功后标记上游健康；失败时标记上游不健康并返回错误。

所有 HTTP 请求都使用传入的 `context.Context`。当 context 取消或超时时，流程立即返回，不继续重试。

## 错误处理与重试

验证码登录尝试最多 3 次。重试只覆盖验证码和 OCR 这类不稳定环节，不覆盖明确的账号密码错误。

提交登录响应判断规则：

- 包含 `密码错误`、`用户名或密码错误`、`用户名密码错误`、`您提供的用户名或者密码有误`：返回 `账号或密码错误`，终止登录。
- 包含 `验证码错误` 或 `验证码不正确`：进入下一次验证码尝试。
- body 为空，或包含 `正在登录`、`location`、`教学一体化服务平台`：视为提交阶段通过，继续最终验证。
- 其他响应：提取页面文本，作为未知登录错误返回。

OCR 响应处理规则：

- HTTP 非 2xx：本次 OCR 失败，可重试。
- JSON 解析失败：本次 OCR 失败，可重试。
- 兼容 `{ "code": 200, "data": "abcd", "message": "..." }`；`code` 为 `200` 或 `0` 表示成功。
- 如果响应没有 `code` 字段，则在 `data` 或 `result` 非空时按成功处理，以兼容 ddddocr-fastapi README 示例未明确响应结构的情况。
- 兼容 ddddocr-fastapi 可能返回的 `result` 字段；优先取 `data`，为空时取 `result`。
- 识别结果去除空白后为空：本次 OCR 失败，可重试。

最终验证规则：

- 访问 `http://zhjw.qfnu.edu.cn/jsxsd/framework/jsMain.jsp`。
- HTTP 状态非 200 或响应中没有 `教学一体化服务平台` 时，判定登录失败。
- 成功时调用 `MarkUpstreamHealthy()`。
- 失败时调用 `MarkUpstreamUnhealthy()`，消息保留关键错误原因。

## 配置与文档

新增环境变量：

- `OCR_URL`：OCR 服务基础地址，例如 `http://<生产服务器>:8000`，后端会调用 `${OCR_URL}/ocr`。

需要更新：

- `.env.example`：补充 `OCR_URL` 示例与说明。
- `README.md`：补充部署变量表中的 `OCR_URL`。

## 测试计划

新增或更新 Go 单元测试：

1. `encoded` 凭证算法测试：使用固定 `username/password/scode/sxh`，验证输出完全符合插入规则。
2. OCR 响应解析测试：覆盖成功、失败码、空结果、`result` 兼容、字符串/数字 code 兼容。
3. 错误分类测试：覆盖账号密码错误、验证码错误、疑似成功、未知错误。

实现完成后运行：

```bash
go test ./...
```

如生产配置允许，再用真实 `OCR_URL` 与测试账号进行一次启动登录验证。

## 验收标准

- 未配置 `OCR_URL` 时，后端登录失败信息明确指出缺少 OCR 配置。
- 配置 `OCR_URL` 后，`client.Login` 使用强智直登流程，不再调用旧 CAS 登录页和 `checkNeedCaptcha.htl`。
- OCR 验证码错误会自动重试，最多 3 次。
- 账号密码错误不会重试，直接返回明确错误。
- 登录成功后，现有空教室查询接口继续通过同一个 `cas.Client` 发起请求。
- `go test ./...` 通过。
