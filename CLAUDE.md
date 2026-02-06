# Drip - 内网穿透项目架构文档

## 项目概述

Drip 是一个高性能的自托管内网穿透解决方案，使用 Go 语言开发。支持 HTTP/HTTPS/TCP 隧道，采用 TLS 1.3 和 WebSocket 双模式传输。

- **仓库**: https://github.com/zxcHolmes/drip
- **原始项目**: https://github.com/Gouryella/drip
- **许可证**: BSD 3-Clause
- **语言**: Go 1.21+

## 核心架构

### 1. 统一二进制设计

```
drip (单一可执行文件)
├── drip server          # 服务器模式
├── drip http <port>     # HTTP 隧道客户端
├── drip https <port>    # HTTPS 隧道客户端
├── drip tcp <port>      # TCP 隧道客户端
└── drip config          # 配置管理
```

### 2. 技术栈分层

```
┌─────────────────────────────────────────────────┐
│  应用层 (HTTP/HTTPS/TCP 流量)                    │
└─────────────────────────────────────────────────┘
                     ↓
┌─────────────────────────────────────────────────┐
│  Yamux 多路复用层                                │
│  github.com/hashicorp/yamux v0.1.2              │
│  - 在单个连接上创建多个虚拟流                     │
│  - 流控制、窗口管理 (512KB 窗口)                 │
│  - 背压机制防止内存溢出                          │
└─────────────────────────────────────────────────┘
                     ↓
┌─────────────────────────────────────────────────┐
│  自定义帧协议 (Drip Protocol)                    │
│  内部实现: internal/shared/protocol/             │
│  - 帧头: [4字节长度][1字节类型]                  │
│  - 帧类型: Register, Heartbeat, DataConnect...   │
│  - 最大帧大小: 1MB (防 DoS)                      │
└─────────────────────────────────────────────────┘
                     ↓
┌─────────────────────────────────────────────────┐
│  net.Conn 适配层                                 │
│  内部实现: internal/shared/wsutil/conn.go        │
│  - WebSocket → net.Conn 适配器                   │
│  - 隐藏 WebSocket 消息边界                       │
│  - 只使用 BinaryMessage                          │
└─────────────────────────────────────────────────┘
                     ↓
┌─────────────────────────────────────────────────┐
│  传输层 (双模式)                                 │
│  模式 1: TLS 1.3 直连 (默认, 性能最优)           │
│  模式 2: WebSocket over TLS (CDN 友好)          │
│  github.com/gorilla/websocket v1.5.3            │
└─────────────────────────────────────────────────┘
                     ↓
┌─────────────────────────────────────────────────┐
│  TCP 层 (标准库 net)                             │
└─────────────────────────────────────────────────┘
```

### 3. 双模式传输机制

#### 模式一：TLS 1.3 直连
```
客户端 --[TLS 1.3]--> 服务器:8443
         (直接连接，性能最优)
```

**实现位置**: `internal/client/tcp/connection_dialer.go:86-108`

**特点**:
- 强制 TLS 1.3，拒绝较低版本
- TCP 优化：NoDelay, KeepAlive, 256KB 缓冲区
- 适用于直接暴露服务器端口的场景

#### 模式二：WebSocket over TLS
```
客户端 --[WSS]--> CDN --[WSS]--> 服务器:443
         (路径: wss://server/_drip/ws)
```

**实现位置**:
- 客户端: `internal/client/tcp/connection_dialer.go:111-154`
- 服务器: `internal/server/proxy/websocket_handler.go`

**特点**:
- 可穿透 Cloudflare 等 CDN
- 使用 443 端口，防火墙友好
- 30 秒 Ping 保持连接活跃

#### 自适应传输选择

```go
// 三种模式
TransportAuto       = "auto"    // 自动探测
TransportTCP        = "tcp"     // 强制 TLS
TransportWebSocket  = "wss"     // 强制 WebSocket

// 探测流程
1. 客户端访问 https://server/_drip/discover
2. 服务器返回: {"transports": ["tcp", "wss"], "preferred": "tcp"}
3. 客户端根据响应选择最佳传输方式
```

**实现位置**: `internal/client/tcp/connection_dialer.go:156-197`

### 4. 连接建立流程

```
第一阶段：注册隧道
─────────────────────────────────────────────
客户端                              服务器
  │                                  │
  ├─[FrameTypeRegister]────────────>│
  │  {                               │
  │    token: "xxx",                 │
  │    subdomain: "myapp",           │
  │    tunnel_type: "http",          │
  │    pool_capabilities: {...}      │
  │  }                               │
  │                                  │
  │<─────────[FrameTypeRegisterAck]─┤
  │  {                               │
  │    subdomain: "myapp",           │
  │    url: "https://myapp.xxx.com", │
  │    tunnel_id: "uuid",            │
  │    recommended_conns: 4          │
  │  }                               │
  │                                  │

第二阶段：建立 Yamux 会话
─────────────────────────────────────────────
  │                                  │
  ├─[Yamux Client Session]──────────┤
  │                                  │
  │  (在同一个 TCP/WS 连接上)        │
  │  yamux.Server(conn, config)      │
  │                                  │

第三阶段：建立数据连接池（可选）
─────────────────────────────────────────────
  │                                  │
  ├─[新连接 1]─────────────────────>│
  │  FrameTypeDataConnect            │
  │  {tunnel_id: "uuid"}             │
  │                                  │
  ├─[新连接 2]─────────────────────>│
  │  FrameTypeDataConnect            │
  │                                  │
  ├─[新连接 3]─────────────────────>│
  │  FrameTypeDataConnect            │
  │                                  │
  └─ (最多 15 个并行连接)            │
```

**实现位置**:
- 注册处理: `internal/server/tcp/registration_handler.go`
- 客户端连接: `internal/client/tcp/pool_client.go:191-327`

### 5. HTTP 请求转发流程

```
外部用户
  │
  │ GET https://myapp.tunnel.com/api/users
  ↓
┌──────────────────────────────────────┐
│ 服务器 (internal/server/proxy/)       │
│                                      │
│ 1. 解析 Host 头: myapp.tunnel.com    │
│ 2. 提取子域名: myapp                 │
│ 3. FNV 哈希到分片: shard[5]          │
│ 4. 查找隧道: manager.Get("myapp")    │
│ 5. 获取 Yamux 流 (3秒超时)           │
│ 6. 将 HTTP 请求写入流                │
│    r.Write(stream)                   │
│ 7. 从流读取响应                      │
│    http.ReadResponse(stream, r)      │
│ 8. 复制响应头 (过滤 hop-by-hop)      │
│ 9. 流式复制响应体 (32KB 分块)        │
│    io.CopyBuffer(w, resp.Body, buf)  │
└──────────────────────────────────────┘
  │
  │ (通过 Yamux 流 + WebSocket/TLS)
  ↓
┌──────────────────────────────────────┐
│ 客户端 (internal/client/tcp/)         │
│                                      │
│ 1. 从 Yamux 流读取 HTTP 请求         │
│    http.ReadRequest(stream)          │
│ 2. 构建到本地服务的请求              │
│    http://127.0.0.1:3000/api/users   │
│ 3. 添加转发头                        │
│    X-Forwarded-Host, X-Forwarded-Proto│
│ 4. 发送到本地服务 (连接池)           │
│    httpClient.Do(req)                │
│ 5. 将响应写回流 (分块传输)           │
│    32KB 缓冲区循环                   │
└──────────────────────────────────────┘
  │
  ↓
本地服务 (localhost:3000)
```

**关键实现**:
- 服务器: `internal/server/proxy/handler.go:141-318`
- 客户端: `internal/client/tcp/pool_handler.go:63-154`

### 6. 大文件流式传输

```
100MB 视频文件传输示例
─────────────────────────────────────────────

本地服务 (100MB)
  │
  ├─ 读取 32KB ────┐
  │                │
  ↓                ↓
客户端 HTTP Client  Yamux 流
  │                │
  │ 窗口控制       │ (512KB 窗口)
  │ 背压机制       │ (窗口满时阻塞)
  │                │
  ↓                ↓
WebSocket/TLS     服务器
  │                │
  │ 分帧传输       │ (自动分帧)
  │                │
  ↓                ↓
外部客户端      io.CopyBuffer
                   │
                   └─ 32KB 分块写入

总内存占用: ~1MB (不是 100MB!)
- Yamux 窗口: 512KB
- 传输缓冲区: 32KB-64KB
- 其他缓冲: ~100KB
```

**内存优化**:
- **对象池**: `internal/shared/pool/buffer_pool.go`
  - SizeSmall: 8KB
  - SizeMedium: 32KB
  - SizeLarge: 64KB
- **零拷贝**: 使用 `net.Buffers` 实现 `writev` 系统调用
- **流式处理**: 从不缓存整个文件

### 7. 并发优化：分片锁

```go
// 传统设计：全局锁 (高竞争)
type Manager struct {
    mu      sync.RWMutex
    tunnels map[string]*Connection
}

// Drip 设计：分片锁 (低竞争)
type Manager struct {
    shards [32]shard  // 32 个独立分片
}

type shard struct {
    tunnels map[string]*Connection
    mu      sync.RWMutex
}

// FNV-1a 哈希分配
func (m *Manager) getShard(subdomain string) *shard {
    h := fnv.New32a()
    h.Write([]byte(subdomain))
    return &m.shards[h.Sum32()%32]
}
```

**性能提升**:
- 锁竞争减少 32 倍
- 并发查询可以同时访问不同分片
- 适合高并发场景

**实现位置**: `internal/server/tunnel/manager.go:128-133`

### 8. 连接池架构

```
┌─────────────────────────────────────────┐
│ 主连接 (Primary Connection)             │
│ - 负责注册和心跳                         │
│ - 失败时整个隧道关闭                     │
│ - Yamux 会话                            │
└─────────────────────────────────────────┘
           │
           ├─ Stream 1 (处理请求 A)
           ├─ Stream 2 (处理请求 B)
           └─ Stream N (并发处理)

┌─────────────────────────────────────────┐
│ 数据连接 1 (Data Connection)             │
│ - 额外的 Yamux 会话                      │
│ - 并行处理请求                           │
└─────────────────────────────────────────┘
           │
           ├─ Stream 1
           └─ Stream N

┌─────────────────────────────────────────┐
│ 数据连接 2-15                            │
│ - 动态扩缩容                             │
│ - 根据负载自动调整                        │
└─────────────────────────────────────────┘

总吞吐量 = 主连接 + 数据连接 1-15
```

**自动扩缩容**:
- 最小连接: 2 (主连接 + 1个数据连接)
- 默认连接: 4
- 最大连接: 16 (主连接 + 15个数据连接)
- 根据活跃流数量动态调整

**实现位置**: `internal/client/tcp/session_scaler.go`

### 9. 安全特性

```yaml
传输层安全:
  - 强制 TLS 1.3
  - 拒绝 TLS 1.2 及以下版本
  - 256 位密钥

应用层安全:
  - Token 认证 (Bearer Token)
  - IP 白名单/黑名单
  - 代理认证 (密码/Bearer)
  - 速率限制 (10次/分钟/IP)

DoS 防护:
  - 最大帧大小: 1MB
  - 最大隧道数: 1000
  - 每 IP 最大隧道: 10
  - 请求超时: 30秒
  - 流打开超时: 3秒

协议安全:
  - Yamux 窗口控制 (512KB)
  - 心跳检测 (2秒间隔, 6秒超时)
  - 连接泄漏检测
```

### 10. 性能指标

```yaml
TCP 优化:
  - NoDelay: true (禁用 Nagle)
  - KeepAlive: 30s
  - 读缓冲: 256KB
  - 写缓冲: 256KB

HTTP 优化:
  - HTTP/2 支持
  - MaxConcurrentStreams: 1000
  - 连接池化
  - 禁用压缩 (减少 CPU)

Yamux 优化:
  - AcceptBacklog: 8192
  - StreamWindow: 512KB
  - KeepAlive: 15s

内存优化:
  - 对象池复用
  - 流式传输
  - 零拷贝优化
```

## 目录结构

```
drip/
├── cmd/drip/                    # 主程序入口
│   └── main.go
│
├── internal/
│   ├── client/                  # 客户端实现
│   │   ├── cli/                 # CLI 命令
│   │   │   ├── root.go         # 根命令
│   │   │   ├── server.go       # 服务器命令
│   │   │   ├── http.go         # HTTP 隧道
│   │   │   ├── https.go        # HTTPS 隧道
│   │   │   ├── tcp.go          # TCP 隧道
│   │   │   └── config.go       # 配置管理
│   │   └── tcp/                 # TCP 客户端核心
│   │       ├── connector.go     # 连接器 (双模式)
│   │       ├── connection_dialer.go  # 连接拨号
│   │       ├── pool_client.go   # 连接池客户端
│   │       ├── pool_handler.go  # 请求处理
│   │       └── session_scaler.go # 自动扩缩容
│   │
│   ├── server/                  # 服务器实现
│   │   ├── proxy/              # HTTP 代理
│   │   │   ├── handler.go      # 主处理器
│   │   │   ├── websocket_handler.go  # WebSocket 隧道
│   │   │   └── auth_handler.go # 认证处理
│   │   ├── tcp/                # TCP 服务器
│   │   │   ├── listener.go     # TCP 监听器
│   │   │   ├── connection.go   # 连接处理
│   │   │   ├── registration_handler.go  # 注册处理
│   │   │   └── port_allocator.go # 端口分配
│   │   └── tunnel/             # 隧道管理
│   │       ├── manager.go      # 分片锁管理器
│   │       └── connection.go   # 隧道连接
│   │
│   └── shared/                  # 共享组件
│       ├── protocol/           # 协议定义
│       │   ├── frame.go        # 帧协议
│       │   ├── messages.go     # 消息定义
│       │   └── adaptive.go     # 自适应传输
│       ├── wsutil/             # WebSocket 工具
│       │   └── conn.go         # net.Conn 适配器 ⭐
│       ├── pool/               # 对象池
│       │   ├── buffer_pool.go  # 缓冲区池
│       │   └── worker_pool.go  # 工作池
│       ├── netutil/            # 网络工具
│       │   └── pipe.go         # 双向管道
│       └── mux/                # Yamux 配置
│           └── session_builder.go
│
├── pkg/config/                 # 配置管理
│   └── config.go
│
├── Makefile                    # 构建脚本
├── go.mod                      # Go 模块
└── README.md                   # 项目说明
```

## 关键代码文件

### 必读文件 (按优先级)

1. **`internal/shared/wsutil/conn.go`** ⭐⭐⭐⭐⭐
   - WebSocket → net.Conn 适配器
   - 整个架构的关键抽象层
   - 只有 80 行代码，但非常重要

2. **`internal/client/tcp/connection_dialer.go`** ⭐⭐⭐⭐⭐
   - 双模式传输选择
   - 自动探测服务器能力
   - TLS/WebSocket 连接建立

3. **`internal/server/tunnel/manager.go`** ⭐⭐⭐⭐
   - 分片锁实现
   - 隧道注册和查找
   - 并发优化的典范

4. **`internal/server/proxy/handler.go`** ⭐⭐⭐⭐
   - HTTP 请求路由
   - Host 头解析
   - 流式转发

5. **`internal/client/tcp/pool_handler.go`** ⭐⭐⭐⭐
   - 客户端请求处理
   - HTTP/TCP 转发逻辑
   - WebSocket 升级

6. **`internal/shared/protocol/frame.go`** ⭐⭐⭐
   - 自定义帧协议
   - 零拷贝优化

7. **`internal/client/tcp/pool_client.go`** ⭐⭐⭐
   - 连接池管理
   - Yamux 会话维护
   - 心跳机制

## 编译和部署

### 编译

```bash
# 编译当前平台
make build
# 输出: bin/drip

# 编译所有平台
make build-all
# 输出:
#   bin/drip-linux-amd64
#   bin/drip-linux-arm64
#   bin/drip-darwin-amd64
#   bin/drip-darwin-arm64
#   bin/drip-windows-amd64.exe
```

### 启动服务器

```bash
# 使用自签名证书 (测试)
./bin/drip server \
  --domain tunnel.localhost \
  --port 8443 \
  --token your-secret-token

# 使用 Let's Encrypt (生产)
./bin/drip server \
  --domain tunnel.example.com \
  --port 443 \
  --token your-secret-token \
  --tls-cert /etc/letsencrypt/live/tunnel.example.com/fullchain.pem \
  --tls-key /etc/letsencrypt/live/tunnel.example.com/privkey.pem
```

### 启动客户端

```bash
# 配置 (首次)
./bin/drip config init

# HTTP 隧道
./bin/drip http 3000

# 自定义子域名
./bin/drip http 3000 --subdomain myapp

# TCP 隧道
./bin/drip tcp 5432
```

## 设计模式和最佳实践

### 1. 适配器模式
将 WebSocket 适配为 net.Conn 接口，让上层代码完全无感知。

### 2. 对象池模式
复用缓冲区和 Reader，减少 GC 压力。

### 3. 分片模式
使用 32 个分片减少锁竞争，提高并发性能。

### 4. 生产者-消费者模式
Yamux 流池，生产者打开流，消费者等待并处理。

### 5. 流式处理
从不缓存整个文件，数据像水流一样通过各层。

### 6. 错误处理
- 使用 CAS 原子操作保证一致性
- 分层回滚机制
- 详细的错误日志

### 7. 监控和可观测性
- Prometheus 指标
- 结构化日志 (zap)
- 性能分析 (pprof)

## 依赖库

```go
// 核心依赖
github.com/hashicorp/yamux v0.1.2         // TCP 多路复用
github.com/gorilla/websocket v1.5.3       // WebSocket 实现
github.com/spf13/cobra v1.10.2            // CLI 框架
go.uber.org/zap v1.27.1                   // 日志库
github.com/prometheus/client_golang       // 监控指标

// 工具库
github.com/goccy/go-json v0.10.5          // 快速 JSON
github.com/charmbracelet/lipgloss v1.1.0  // 终端 UI
golang.org/x/crypto                       // 加密库
golang.org/x/net                          // 网络库
```

## 性能测试建议

```bash
# 基准测试
make bench

# 覆盖率测试
make test-coverage

# 性能分析
./bin/drip server --pprof 6060
# 访问 http://localhost:6060/debug/pprof/
```

## 改进空间

1. **UDP 支持**: 目前只支持 TCP，可以添加 UDP 隧道
2. **QUIC 传输**: 比 TLS 更低延迟
3. **Web UI**: 目前只有 CLI
4. **负载均衡**: 多后端支持
5. **插件系统**: 扩展功能

## 推送到仓库

```bash
# 查看状态
git status

# 添加所有更改
git add .

# 提交
git commit -m "Add CLAUDE.md architecture documentation"

# 推送到你的 fork
git push fork main
```

## 学习路径

### 初学者
1. 阅读 `internal/shared/wsutil/conn.go` (理解适配器模式)
2. 阅读 `internal/shared/protocol/frame.go` (理解帧协议)
3. 运行 `make run-server` 和 `make run-client` (本地测试)

### 中级
1. 阅读 `internal/client/tcp/connection_dialer.go` (理解双模式)
2. 阅读 `internal/server/tunnel/manager.go` (理解分片锁)
3. 添加自定义功能并测试

### 高级
1. 阅读完整的请求转发流程
2. 性能调优和基准测试
3. 添加新的传输协议 (如 QUIC)

## 参考资源

- [Yamux 协议规范](https://github.com/hashicorp/yamux/blob/master/spec.md)
- [TLS 1.3 RFC](https://tools.ietf.org/html/rfc8446)
- [WebSocket RFC](https://tools.ietf.org/html/rfc6455)
- [Go net.Conn 接口](https://pkg.go.dev/net#Conn)

---

**维护者**: @zxcHolmes
**最后更新**: 2026-02-06
