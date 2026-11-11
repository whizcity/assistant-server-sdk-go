# assistant-server-sdk-go

Python assistant-server 的 Go SDK：HMAC 签名、审计事件上报、查询，以及可选的 GORM v2 自动审计。要求 Go 1.25+。

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
    // Timeout 缺省 20s；也可传入自定义 HTTP client。
})

// 单条同步
r := client.Sync(map[string]any{
    "event_id": as.NewULID(), "service": "parking", "action": "update",
    "resource": "orders", "resource_id": "5",
})

// 批量同步：超过 500 条会自动拆成多次请求
r = client.SyncBatch(events)

// 查询/回拉、单条读取
r = client.Query(map[string]any{"service": "parking", "action": "update", "page_size": 20})
r = client.Get("01HV4ZJ3KF7XY98ZXNQFS11JJ9", nil)

if !r.Success() {
    // r.Code / r.Message；r.Err() 提供 error 风格
}
```

## 任意接口

`Request(method, path, params)` 是通用入口：POST/PUT/PATCH 把 params 当 JSON body（HTML 转义关闭、UTF-8 原样），其余方法当 query（参与签名）。

```go
client.Request("POST", "/api/v1/whatever/new", params)
```

`RequestMultipart(method, path, fileField, filename, fileContent, extraForm)` 手工预生成完整 multipart 字节再签名（Content-SHA256 = 实发包），OCR `POST /api/v1/ocr` 可直接用它。

## 批量合并助手

多行写操作可折叠成一条审计事件 `{"count":N,"rows":...}`；超过 16KB 预算（每行 +16 字节开销）自动拆成多事件：

```go
events := as.MergeInsertEvents("parking", "audit_subjects", rows, as.EventProfile{OperatorID: "9"})
events = append(events, as.MergeDeleteEvents("parking", "orders", images, prof)...)
events = append(events, as.MergeUpdateEvents("parking", "orders", pairs, prof)...)
r := client.SyncBatch(events)
```

- rows 形态：insert=list，delete/update=按 id 键的 map（update 值为 `{before, after}`）。
- 相同 trace_id 的分片即同一批操作；每条 `created_at` 均为 UTC。

## GORM v2 自动审计

`auditgorm` 是同一模块下的子包，依赖 `gorm.io/gorm`（v1.25+）。本地开发时在业务模块里替换根模块即可：

```go
require github.com/whizcity/assistant-server-sdk-go v0.0.0

replace github.com/whizcity/assistant-server-sdk-go => ../assistant-server-sdk-go
```

注册一次：

```go
import (
    "context"
    as "github.com/whizcity/assistant-server-sdk-go"
    "github.com/whizcity/assistant-server-sdk-go/auditgorm"
)

type profileKey struct{}

err := auditgorm.Register(db, recorder, func(ctx context.Context) as.EventProfile {
    p, _ := ctx.Value(profileKey{}).(as.EventProfile)
    return p
}, nil) // 第三个参数可传 func(table string) bool 过滤表
```

`recorder` 的签名是 `func(tx *gorm.DB, events []map[string]any) error`。它在业务写入的**同一事务**内同步调用，通常把事件写进本地 `audit_logs` 出站表；返回错误会让业务写入回滚。SDK 不负责 HTTP 同步/重试，业务侧用定时任务读取未同步行并调用 `client.SyncBatch`，成功后标记，失败按自己的策略退避。`audit_logs` 表本身不会被自动审计。

`Register` 的 profile 从 `tx.Statement.Context` 取上下文；**写操作必须带上请求 context**，否则 `OperatorID` 为空，事件会被静默跳过：

```go
db := db.WithContext(ctx) // 请求入口或 service 构造时
db.Create(&row)
db.Model(&row).Updates(map[string]any{"state": "paid"})
db.Where("id IN ?", ids).Delete(&Row{})
```

如果写操作会在 HTTP 响应结束后继续（后台 goroutine），用 `db.WithContext(context.WithoutCancel(ctx))`：保留操作人信息，但不会被请求取消。

自动捕获：

- `Create`（struct 或 slice）
- 单行 `Save` / `Model(&row).Updates(...)`
- 带条件的批量 `Update` / `Delete`

不捕获（需手动 `as.NewEvent` 或 `Merge*`）：raw `Exec`、目标为 map 的 `Create`、无条件的全表写。

事件在 SQL 成功后、事务提交前交给 recorder，因此本地审计行和业务写入同生共死；HTTP 上报由业务侧的定时同步任务完成，失败的行保留在本地重试。

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
client := as.NewClient(as.ClientConfig{
    // ...
    DebugLog: func(f string, a ...any) { log.Printf(f, a...) },
})
```

调试日志默认关。

## 运行测试

```bash
go test ./...
```
