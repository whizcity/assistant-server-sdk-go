# assistant-server-sdk-go

Python assistant-server 的 Go SDK。零依赖（仅 stdlib），`go 1.14` 以上可用，与中心服务共享一套 HMAC 签名与接口约定。

```bash
go get github.com/whizcity/assistant-server-sdk-go
```

## 快速开始

```go
import as "github.com/whizcity/assistant-server-sdk-go"

client := as.NewClient(as.ClientConfig{
    Address: "http://assistant-server:8080", // 不带路径、不带末尾斜杠
    AppID:   "your-app-id",
    Secret:  "your-secret",
})

// 单条同步
r := client.Sync(map[string]any{
    "event_id": as.NewULID(), "service": "parking", "action": "update",
    "resource": "orders", "resource_id": "5",
})

// 批量同步（events 信封，1..500 条，服务端按 event_id 去重）
r = client.SyncBatch(events)

// 查询/回拉、单条读取
r = client.Query(map[string]any{"service": "parking", "action": "update", "size": 20})
r = client.Get("01HV4ZJ3KF7XY98ZXNQFS11JJ9", nil)

if !r.Success() {
    // r.Code / r.Message；r.Err() 提供 error 风格
}
```

## 任意接口

服务端加新接口不用等 SDK：`Request(method, path, params)` 是通用入口——POST/PUT/PATCH 把 params 当 JSON body（HTML 转义关闭、UTF-8 原样，与中心服务的 JSON 口径一致），其余方法当 query（参与签名）。

```go
client.Request("POST", "/api/v1/whatever/new", params)
```

`RequestMultipart(method, path, fileField, filename, fileContent, extraForm)` 手工预生成完整 multipart 字节再签名（Content-SHA256 = 实发包），service 端验签口径一致；OCR `POST /api/v1/ocr` 可直接用它。

## 批量合并

模型钩子不存在的 Go 侧改为显式命名空间助手：多行写操作折叠成一条审计事件 `{"count":N,"rows":...}`，超出 16KB 预算（每行 +16 字节开销）自动拆成多事件且零丢失：

```go
events := as.MergeInsertEvents("parking", "audit_subjects", rows, as.EventProfile{OperatorID: "9"})
events = append(events, as.MergeDeleteEvents("parking", "orders", images, prof)...)
events = append(events, as.MergeUpdateEvents("parking", "orders", pairs, prof)...)
r := client.SyncBatch(events)
```

- rows 形态：insert=list，delete/update=按 id 键的 map（update 值为 `{before, after}`）。
- 相同 trace_id 的分片即同一批操作；列表内每条 `created_at` 均为 UTC。

## 响应语义

| 情况 | Result |
|---|---|
| body 无 `code` 字段 | 成功；`Data` 取 `data` 键或整个 body |
| body 带 `code` | 失败，透传 `code`/`message` |
| 非 JSON body | 失败，`code`=HTTP 状态，`message`=前 500 字节 |
| 缺 `Address`/`AppID`/`Secret` | `CONFIG_ERROR`（不发请求） |
| 传输失败 | `INTERNAL_ERROR`（`DebugLog` 可选接日志） |

## 调试

```go
client, _ := NewClient(ClientConfig{..., DebugLog: func(f string, a ...any) { log.Printf(f, a...) }})
```

调试日志默认关。

## 运行测试

```bash
go test ./...
```
