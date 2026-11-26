# 风传 API

风传提供面向脚本和自动化程序的本机 HTTP API。API 使用独立密钥，不再复用网页登录密码。

服务首次启动时会生成 `data/api-key`。也可以显式设置：

```powershell
$env:FENGCHUAN_API_KEY = "your-independent-api-key"
```

或启动参数：

```text
fengchuan.exe -listen :8099 -password "<登录密码>" -api-key "<独立API密钥>"
```

请求时使用以下任一请求头：

```http
Authorization: Bearer <API_KEY>
```

```http
X-FengChuan-Key: <API_KEY>
```

网页登录密码不能用于这些请求头。认证失败会返回 `401`；连续失败会触发临时 `429` 限流。

## 健康检查

无需认证：

```bash
curl http://127.0.0.1:8099/api/health
```

```json
{
  "ok": true,
  "name": "FengChuan",
  "version": "v1.15.0"
}
```

## 上传文件并附带说明

`POST /api/upload` 使用 `multipart/form-data`，请求体上限为 2 GiB。

| 字段 | 必需 | 说明 |
| --- | --- | --- |
| `file` | 是 | 文件字段，可重复；也接受 `files` 或 `file-upload` |
| `note` | 否 | 同时发送一条 Markdown 更新说明 |
| `note_name` | 否 | 说明文件名，默认自动生成 |
| `sender` | 否 | 发送者名称，默认 `API` |
| `expiry` | 否 | 过期时间，默认 `Never` |

没有文件时，必须提供 `note`。

```bash
curl -f \
  -H "Authorization: Bearer $FENGCHUAN_API_KEY" \
  -F "file=@dist/app.apk" \
  -F "file=@dist/app-x86_64.apk" \
  -F "sender=Codex" \
  -F "note=修复了登录问题并更新了安装包。" \
  -F "note_name=更新说明.md" \
  http://127.0.0.1:8099/api/upload
```

响应中的 ID 与下载地址统一使用 `/`：

```json
{
  "ok": true,
  "files": [
    {
      "id": "files/app.apk",
      "filename": "app.apk",
      "size": 123456,
      "download_url": "/download/files%2Fapp.apk"
    }
  ],
  "note": {
    "id": "text/更新说明.md",
    "filename": "更新说明.md",
    "size": 42,
    "download_url": "/download/text%2F%E6%9B%B4%E6%96%B0%E8%AF%B4%E6%98%8E.md"
  }
}
```

## 发送文本消息

`POST /api/message`

```bash
curl -f \
  -H "Authorization: Bearer $FENGCHUAN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"content":"构建已完成","sender":"Codex","name":"构建结果.txt"}' \
  http://127.0.0.1:8099/api/message
```

```json
{
  "ok": true,
  "id": "text/构建结果.txt",
  "filename": "构建结果.txt"
}
```

## PowerShell 客户端

仓库提供 [tools/fengchuan-upload.ps1](../tools/fengchuan-upload.ps1)：

```powershell
$env:FENGCHUAN_API_KEY = "<独立API密钥>"
$env:FENGCHUAN_SENDER = "Codex-风传API"
powershell -NoProfile -ExecutionPolicy Bypass `
  -File .\tools\fengchuan-upload.ps1 `
  -BaseUrl http://127.0.0.1:8099 `
  -File .\dist\app.apk, .\dist\app-x86_64.apk `
  -Sender Codex `
  -Note "构建完成，附件已更新。" `
  -NoteName "更新说明.md"
```

脚本也会尝试读取自身目录下的 `data/api-key`。脚本输出版本、文件名、大小和下载地址 JSON。

未指定 `-Sender` 时，来源名称按以下顺序生成：

1. `FENGCHUAN_SENDER`
2. `CODEX_THREAD_NAME`
3. `CODEX_THREAD_ID` 前 8 位
4. `Codex`

## 状态码

| 状态码 | 含义 |
| --- | --- |
| `200` | 操作成功 |
| `400` | 请求字段为空、格式错误或路径非法 |
| `401` | API Key 缺失或错误 |
| `413` | 请求体超过上限 |
| `429` | 认证失败次数过多，稍后重试 |
| `500` | 服务端保存文件失败 |
