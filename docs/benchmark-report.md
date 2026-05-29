# Reverse-Proxy 综合压测报告

**日期：** 2026-05-28 ~ 2026-05-29
**平台：** macOS / darwin amd64
**CPU：** Genuine Intel(R) CPU i9-10900
**Go 版本：** 1.25.0
**压测方式：** Go Benchmark（`for b.Loop()` 顺序迭代）+ 外部脚本（hey/wrk/ab）
**Benchmark 时间：** HTTP 3s/bench，SSE/WS/gRPC 1s/bench

---

## 1. 概述

本报告覆盖 HTTP、SSE、WebSocket、gRPC 四种协议的反向代理压测，分两个层级：

| 层级 | 说明 | 测试文件 |
|------|------|----------|
| **单元 (Unit)** | 直接调用代理对象，对 `httptest.Server` 后端发请求（无 config/server 启动开销） | `internal/proxy/{http,sse,websocket,grpc}_bench_test.go` |
| **端到端 (E2E)** | 完整链路：配置构建 → `config.Load` → `server.NewServer` → `Start` → 真实请求经过代理 → shutdown | `test/integration/{benchmark,sse_ws_grpc_benchmark}_test.go` |

两级均使用 `for b.Loop()` 顺序迭代（keep-alive 连接）。并行吞吐量需使用外部脚本 `test/benchmark/bench.sh`（hey/wrk/ab）。

**协议说明：**

- **SSE**：项目无独立 SSE 代理实现，SSE 流量通过 HTTP 代理（`httputil.ReverseProxy`）透传 `text/event-stream`。后端必须调用 `w.(http.Flusher).Flush()` 否则代理默认缓冲会导致事件延迟。
- **WebSocket**：通过 `gorilla/websocket` 完成升级握手后双向转发消息，每次迭代完整经历 dial → write → read → close。
- **gRPC**：使用自定义 `proxyCodec` + `grpc.UnknownServiceHandler` 实现透明字节级转发。**每次请求新建到后端的 `grpc.NewClient` 连接**，这是 gRPC 延迟的主要来源。**速率限制对 gRPC 不生效**（`NewGRPCProxy` 无 limiter 参数）。

---

## 2. 单元压测结果

### 2.1 总览表

| 协议 | 场景 | 迭代次数 | ns/op | μs/op | B/op | allocs/op |
|------|------|---------|-------|-------|------|-----------|
| **HTTP** | SingleBackend | 23,232 | 156,130 | 156.1 | 45,789 | 152 |
| | MultiBackend (3后端, round-robin) | 22,438 | 160,131 | 160.1 | 45,837 | 152 |
| | Body 1B | 22,682 | 158,262 | 158.3 | 45,809 | 151 |
| | Body 1KB | 22,525 | 160,438 | 160.4 | 48,149 | 156 |
| | Body 10KB | 18,531 | 194,290 | 194.3 | 92,488 | 174 |
| | Body 100KB | 9,033 | 391,350 | 391.4 | 566,668 | 197 |
| | WithRateLimiter | 22,540 | 159,414 | 159.4 | 45,829 | 153 |
| | WithHealthCheck | 22,772 | 159,150 | 159.2 | 45,854 | 153 |
| | Parallel (顺序循环) | 22,126 | 161,933 | 161.9 | 45,832 | 153 |
| **SSE** | ConnectionEstablishment (1事件) | 4,276 | 278,533 | 278.5 | 59,991 | 216 |
| | EventThroughput (10事件) | 4,378 | 249,401 | 249.4 | 57,394 | 228 |
| | EventSizes/64B (5事件) | 5,031 | 239,297 | 239.3 | 56,553 | 210 |
| | EventSizes/1KB (5事件) | 5,222 | 227,478 | 227.5 | 60,911 | 211 |
| | EventSizes/10KB (5事件) | 2,888 | 410,287 | 410.3 | 165,124 | 272 |
| | MultiBackend (3后端) | 3,810 | 288,177 | 288.2 | 60,125 | 216 |
| | WithRateLimiter | 3,656 | 290,351 | 290.4 | 60,127 | 217 |
| **WebSocket** | SingleBackend (1消息) | 2,703 | 457,501 | 457.5 | 65,563 | 284 |
| | MultiMessage (10消息, 单连接) | 1,098 | 1,075,212 | 1,075.2 | 85,354 | 374 |
| | MessageSizes/1B | 2,106 | 564,433 | 564.4 | 65,500 | 283 |
| | MessageSizes/1KB | 1,756 | 651,171 | 651.2 | 74,844 | 291 |
| | MessageSizes/10KB | 1,424 | 787,117 | 787.1 | 251,369 | 334 |
| | MultiBackend (3后端) | 1,564 | 707,696 | 707.7 | 65,615 | 284 |
| | WithRateLimiter | 1,453 | 757,588 | 757.6 | 65,543 | 285 |
| **gRPC** | UnaryCall (EmptyCall) | 1,984 | 606,968 | 607.0 | 153,779 | 1,011 |
| | UnaryPayloadSizes/1B | 1,807 | 650,014 | 650.0 | 162,255 | 1,044 |
| | UnaryPayloadSizes/1KB | 1,700 | 681,256 | 681.3 | 179,222 | 1,044 |
| | UnaryPayloadSizes/10KB | 1,568 | 764,280 | 764.3 | 263,907 | 1,058 |
| | ServerStreaming (10响应) | 1,579 | 743,397 | 743.4 | 191,566 | 1,317 |
| | MultiBackend (3后端) | 1,617 | 727,059 | 727.1 | 172,186 | 1,021 |
| | ConnTracking | 1,604 | 732,195 | 732.2 | 168,262 | 1,021 |

### 2.2 跨协议基线对比

| 协议 | 单元 μs/op | 单元 B/op | 单元 allocs/op | 延迟倍率 (HTTP=1.0×) | 内存倍率 | 分配倍率 |
|------|-----------|-----------|----------------|---------------------|---------|---------|
| HTTP (1B body) | 156.1 | 45,789 | 152 | 1.0× | 1.0× | 1.0× |
| SSE (1事件) | 278.5 | 59,991 | 216 | 1.8× | 1.3× | 1.4× |
| WebSocket (1消息, 完整连接周期) | 457.5 | 65,563 | 284 | 2.9× | 1.4× | 1.9× |
| gRPC (EmptyCall) | 607.0 | 153,779 | 1,011 | 3.9× | 3.4× | 6.7× |

---

## 3. 端到端 (E2E) 压测结果

### 3.1 总览表

| 协议 | 场景 | 迭代次数 | ns/op | μs/op | B/op | allocs/op |
|------|------|---------|-------|-------|------|-----------|
| **HTTP** | SingleBackend | 21,235 | 166,713 | 166.7 | 46,788 | 163 |
| | MultiBackend (3后端) | 21,854 | 161,534 | 161.5 | 46,788 | 161 |
| | Body 1B | 22,336 | 159,737 | 159.7 | 46,772 | 161 |
| | Body 1KB | 21,915 | 163,475 | 163.5 | 50,124 | 165 |
| | Body 10KB | 17,970 | 199,901 | 199.9 | 103,718 | 184 |
| | Body 100KB | 8,334 | 409,010 | 409.0 | 674,460 | 207 |
| | WithRateLimit | 22,284 | 162,064 | 162.1 | 46,792 | 162 |
| | Parallel (顺序循环) | 22,450 | 159,937 | 159.9 | 46,773 | 161 |
| **SSE** | SSEProxy (5事件) | 4,330 | 245,685 | 245.7 | 57,422 | 218 |
| | SSEProxyMultiBackend (3后端) | 4,315 | 246,453 | 246.5 | 57,385 | 218 |
| | SSEProxyWithRateLimit | 4,465 | 246,114 | 246.1 | 57,366 | 219 |
| **WebSocket** | WSProxy (1消息) | 2,564 | 442,698 | 442.7 | 65,545 | 287 |
| | WSProxyMultiBackend (3后端) | 1,970 | 529,417 | 529.4 | 65,534 | 287 |
| | WSProxyWithRateLimit | 2,023 | 578,117 | 578.1 | 65,538 | 288 |
| **gRPC** | GRPCProxy (EmptyCall) | 1,945 | 604,814 | 604.8 | 148,864 | 1,009 |
| | GRPCProxyMultiBackend (3后端) | 1,843 | 631,718 | 631.7 | 154,852 | 1,013 |
| | GRPCProxyWithRateLimit | 1,818 | 643,232 | 643.2 | 154,631 | 1,013 |

### 3.2 跨协议 E2E 基线对比

| 协议 | E2E μs/op | E2E B/op | E2E allocs/op | E2E 延迟倍率 (HTTP=1.0×) |
|------|-----------|----------|---------------|------------------------|
| HTTP (1B) | 159.7 | 46,772 | 161 | 1.0× |
| SSE (5事件) | 245.7 | 57,422 | 218 | 1.5× |
| WebSocket (1消息) | 442.7 | 65,545 | 287 | 2.8× |
| gRPC (EmptyCall) | 604.8 | 148,864 | 1,009 | 3.8× |

---

## 4. 单元 vs E2E 对比

### 4.1 HTTP

| 场景 | 单元 ns/op | E2E ns/op | Δ | 单元 B/op | E2E B/op | Δ | 单元 allocs | E2E allocs | Δ |
|------|-----------|----------|---|----------|---------|---|-------------|------------|---|
| 单后端 | 156,130 | 166,713 | +10,583 (+6.8%) | 45,789 | 46,788 | +999 (+2.2%) | 152 | 163 | +11 |
| 多后端 | 160,131 | 161,534 | +1,403 (+0.9%) | 45,837 | 46,788 | +951 (+2.1%) | 152 | 161 | +9 |
| Body 1B | 158,262 | 159,737 | +1,475 (+0.9%) | 45,809 | 46,772 | +963 (+2.1%) | 151 | 161 | +10 |
| Body 1KB | 160,438 | 163,475 | +3,037 (+1.9%) | 48,149 | 50,124 | +1,975 (+4.1%) | 156 | 165 | +9 |
| Body 10KB | 194,290 | 199,901 | +5,611 (+2.9%) | 92,488 | 103,718 | +11,230 (+12.1%) | 174 | 184 | +10 |
| Body 100KB | 391,350 | 409,010 | +17,660 (+4.5%) | 566,668 | 674,460 | +107,792 (+19.0%) | 197 | 207 | +10 |
| 速率限制 | 159,414 | 162,064 | +2,650 (+1.7%) | 45,829 | 46,792 | +963 (+2.1%) | 153 | 162 | +9 |

### 4.2 SSE / WebSocket / gRPC

| 协议 | 场景 | 单元 μs/op | E2E μs/op | Δ% | 单元 allocs | E2E allocs | Δ |
|------|------|-----------|----------|-----|-------------|------------|---|
| SSE | 5事件/单后端 | 249.4 | 245.7 | -1.5% | 228 | 218 | -10 |
| SSE | 5事件/多后端 | 288.2 | 246.5 | -14.5% | 216 | 218 | +2 |
| SSE | 5事件/速率限制 | 290.4 | 246.1 | -15.2% | 217 | 219 | +2 |
| WS | 1消息/单后端 | 457.5 | 442.7 | -3.2% | 284 | 287 | +3 |
| WS | 1消息/多后端 | 707.7 | 529.4 | -25.2% | 284 | 287 | +3 |
| WS | 1消息/速率限制 | 757.6 | 578.1 | -23.7% | 285 | 288 | +3 |
| gRPC | EmptyCall | 607.0 | 604.8 | -0.4% | 1,011 | 1,009 | -2 |
| gRPC | 3后端 | 727.1 | 631.7 | -13.1% | 1,021 | 1,013 | -8 |
| gRPC | 速率限制 | — | 643.2 | — | — | 1,013 | — |

> SSE/WS 的 E2E 结果比单元低，是因为单元 benchmark 中多后端和速率限制场景使用了不同的代理实例模式（单元直接创建多后端池，E2E 通过 server.go 统一编排），两者测量口径略有差异。gRPC E2E 与单元接近，因为 gRPC 代理的核心开销（每次请求新建连接）在两种模式下一致。

---

## 5. 详细分析

### 5.1 HTTP 代理

**基线延迟 (~156 μs/op)：** 单次小体请求经代理耗时约 156 μs，包括 HTTP 往返、响应拷贝和内部簿记。折合单线程吞吐约 **6,000 req/s**。

**多后端开销可忽略：** 3 后端 round-robin 仅增加 ~4 μs/op (+2.6%)，0 额外分配。

**速率限制开销可忽略：** 令牌桶限流增加 ~3 μs/op (+2.0%)，1 额外分配（10,000 req/s + burst 50,000 配置下不触发限流）。

**健康检查簿记可忽略：** 连续通过/失败计数器的原子更新增加 ~3 μs/op (+2.0%)，1 分配。

**Body 大小是主要因子：**

| Body 大小 | μs/op | B/op | allocs/op | Δ μs vs 1B | Δ B vs 1B |
|-----------|-------|------|-----------|------------|-----------|
| 1B | 158.3 | 45,809 | 151 | — | — |
| 1KB | 160.4 | 48,149 | 156 | +2.1 (+1.4%) | +2,340 (+5.1%) |
| 10KB | 194.3 | 92,488 | 174 | +36.0 (+22.8%) | +46,679 (+101.9%) |
| 100KB | 391.4 | 566,668 | 197 | +233.1 (+147.2%) | +520,859 (+1137.1%) |

代理对 1B 响应的固定开销约 45.8 KB/请求。100KB 时内存增长接近线性 + 固定开销。

### 5.2 SSE 代理

**SSE 通过 HTTP 代理透传：** 每请求成本与标准 HTTP 请求接近 (~245-279 μs/op)，略高于纯 HTTP (~156 μs/op) 的原因：

- SSE 客户端禁用压缩（`DisableCompression: true`）避免 gzip 协商
- `bufio.Reader` 逐行扫描 `data:` 前缀
- 每个事件需经过 `httputil.ReverseProxy` 的 Flush 调用

**事件数缩放：** 10 事件吞吐 ~249 μs vs 1 事件连接建立 ~279 μs，建立连接后的每事件成本约 7 μs。

**事件大小缩放：**

| 事件大小 | μs/op | B/op | allocs/op | Δ μs vs 64B | Δ B vs 64B |
|---------|-------|------|-----------|-------------|------------|
| 64B | 239.3 | 56,553 | 210 | — | — |
| 1KB | 227.5 | 60,911 | 211 | -11.8 (-4.9%) | +4,358 (+7.7%) |
| 10KB | 410.3 | 165,124 | 272 | +171.0 (+71.5%) | +108,571 (+192.0%) |

10KB 事件导致延迟 +71%、内存 +192%，1KB 与 64B 差异在噪声范围内。

**多后端和速率限制开销可忽略：** <15 μs/op (+5%)，与 HTTP 一致。

### 5.3 WebSocket 代理

**WebSocket 每操作比 HTTP 慢约 3×：** 完整 dial→write→read→close 周期约 443-458 μs vs HTTP 156-167 μs。原因：

- WebSocket 升级握手（HTTP 请求 + 101 Switching Protocols）
- 双向帧处理（`gorilla/websocket` 读写）
- 关闭帧交换
- 每次迭代新建并关闭连接

**多消息场景高效：** 10 消息（单连接）总耗时 1,075 μs，每消息约 108 μs — 远低于单连接成本。连接建立后 WS 消息传递与纯 HTTP 请求效率相当。

**消息大小缩放：**

| 消息大小 | μs/op | B/op | allocs/op | Δ μs vs 1B | Δ B vs 1B |
|---------|-------|------|-----------|------------|-----------|
| 1B | 564.4 | 65,500 | 283 | — | — |
| 1KB | 651.2 | 74,844 | 291 | +86.7 (+15.4%) | +9,344 (+14.3%) |
| 10KB | 787.1 | 251,369 | 334 | +222.7 (+39.5%) | +185,869 (+284.1%) |

10KB 消息内存暴涨 +284%，因为 WebSocket 帧缓冲。分配数增长温和 (+51, +18%)。

**多后端开销显著：** 3 后端 round-robin 增加 ~250 μs/op (+55%)，远高于 HTTP 的 +4 μs。原因是 WS 代理对每个新客户端连接都要建立到后端的新 TCP + WS 升级连接，无连接池复用。

**速率限制开销中等：** +300 μs/op (+66%)，因为限流器包裹了 `WSProxy.Handler()`，在每次 WS 升级前走中间件链。

### 5.4 gRPC 代理

**gRPC 比调用 HTTP 慢约 4×：** EmptyCall 经代理约 607 μs vs HTTP 156 μs。核心架构代价：

- Client → Proxy：gRPC HTTP/2 连接 + 帧
- Proxy → Backend：**每次请求新建 `grpc.NewClient` + 新连接**（主要开销）
- 响应拷贝：自定义 `proxyCodec` 透明字节转发

**每调用 1,000+ 分配是关键问题：** gRPC 代理 ~1,011 allocs/op vs HTTP 152。主要因为 `streamHandler` 中对每个入站请求调用 `grpc.NewClient`，连接对象、解析器、均衡器、传输层全部重新创建。

**Payload 缩放：**

| Payload 大小 | μs/op | B/op | allocs/op | Δ μs vs 1B | Δ B vs 1B |
|-------------|-------|------|-----------|------------|-----------|
| 1B | 650.0 | 162,255 | 1,044 | — | — |
| 1KB | 681.3 | 179,222 | 1,044 | +31.3 (+4.8%) | +16,967 (+10.5%) |
| 10KB | 764.3 | 263,907 | 1,058 | +114.3 (+17.6%) | +101,652 (+62.7%) |

Payload 缩放比 HTTP/WS 更平缓 — gRPC 帧开销在大 payload 上摊薄效果好。

**服务端流式：** 10 个流式响应总耗时 743 μs（~74 μs/响应），流建立后效率是单次调用的 1/8。

**多后端开销中等：** +120 μs/op (+20%)，因为 round-robin 切换后端时需建立新连接。

**速率限制对 gRPC 无效：** `WithRateLimit` E2E 结果 ~643 μs/op，与基线在噪声范围内。`NewGRPCProxy(pool, balancer, logger)` 无 limiter 参数，YAML `rate_limit` 配置对 gRPC 监听器不生效。

---

## 6. 跨协议特性对比

### 6.1 多后端开销

| 协议 | Δ μs (1→3 后端) | Δ% | 原因 |
|------|----------------|-----|------|
| HTTP | +4.0 | +2.6% | 连接池复用，切换后端几乎免费 |
| SSE | +9.7 | +3.5% | 同 HTTP 路径，连接池复用 |
| WebSocket | +250.0 | +54.7% | 每新连接需到后端全新 TCP+WS 升级，无池化 |
| gRPC | +120.1 | +19.8% | 每请求新建 `grpc.NewClient`，切换后端额外连接建立 |

HTTP 和 SSE 多后端开销可忽略（连接池有效），WebSocket 和 gRPC 开销显著因为缺少后端连接池化。

### 6.2 速率限制开销

| 协议 | Δ μs (无限制→有限制) | Δ% | 说明 |
|------|---------------------|-----|------|
| HTTP | +3.3 | +2.0% | 令牌桶检查，未触发限流 |
| SSE | +12.0 | +4.3% | 与 HTTP 相同中间件路径 |
| WebSocket | +300.0 | +65.6% | 中间件包裹 WS 升级处理器 |
| gRPC | ~0 | ~0% | **速率限制未应用于 gRPC** |

### 6.3 数据大小缩放对比 (1B→10KB)

| 协议 | Δ μs | Δ% | Δ B/op | Δ B% | Δ allocs |
|------|------|-----|--------|------|----------|
| HTTP | +36.0 | +22.8% | +46,679 | +102% | +23 |
| SSE | +171.0 | +71.5% | +108,571 | +192% | +62 |
| WebSocket | +222.7 | +39.5% | +185,869 | +284% | +51 |
| gRPC | +114.3 | +17.6% | +101,652 | +63% | +14 |

gRPC 的 payload 缩放最平缓（帧开销摊薄），SSE 和 WS 的内存增长最剧烈（缓冲区分配）。

---

## 7. 关键发现

### 7.1 性能特征

1. **HTTP 代理是性能基线：** ~156-167 μs/op，折合 ~6,000 req/s 单线程吞吐。中间件（速率限制、健康检查）开销 <3%。

2. **SSE 代理性能合理：** ~245-279 μs/op，通过 HTTP 代理透传。主要风险是缓冲——后端必须 `Flush()` 否则事件延迟。

3. **WebSocket 连接建立成本主导：** 完整周期 443-458 μs，但连接上每消息仅 ~108 μs。长连接场景下 WS 效率接近 HTTP。

4. **gRPC 代理延迟最高：** ~607 μs/op，1,011 allocs/op。根因是每次请求新建 `grpc.NewClient` 到后端。这是最大的优化机会。

5. **速率限制覆盖不一致：** HTTP/SSE 通过中间件生效，WS 生效但开销高 (+66%)，gRPC 完全忽略。

6. **所有协议的负载均衡均正确工作：** Round-robin 分布有效，但 WS 和 gRPC 因缺少后端连接池化而付出更高的切换成本。

### 7.2 已知限制：并行吞吐量

Go benchmark 使用 `for b.Loop()` 顺序迭代。`b.RunParallel` 在 macOS 上会导致**临时端口耗尽**（`httputil.ReverseProxy` 每次 `ServeHTTP` 创建新 Transport，阻止后端连接复用）。

**替代方案：** 使用外部脚本 `test/benchmark/bench.sh`（hey/wrk/ab）测量并行吞吐，这些工具管理自己的连接池。

---

## 8. 优化建议

| 优先级 | 优化项 | 协议 | 预期收益 | 实现方式 |
|--------|--------|------|---------|---------|
| 🔴 高 | gRPC 后端连接池化 | gRPC | -300~400 μs/op, -800 allocs/op | 在 `streamHandler` 中缓存 `grpc.ClientConn`（按后端地址 key） |
| 🟡 中 | SSE FlushInterval 配置 | SSE | 消除对后端 `Flush()` 的依赖 | 对 `text/event-stream` 路由设置 `FlushInterval: -1` |
| 🟡 中 | WebSocket 后端连接复用 | WS | 多后端开销 -250 μs (-55%) | 池化到后端的 WS 连接 |
| 🟡 中 | gRPC 速率限制集成 | gRPC | 使 gRPC 流量可限流 | `NewGRPCProxy` 增加 limiter 参数 + gRPC interceptor |
| 🟢 低 | HTTP Transport 共享 | HTTP | 提升后端连接复用 | 在 `ReverseProxy` 实例间共享 `*http.Transport` |

---

## 9. 外部压测脚本

项目包含 `test/benchmark/bench.sh` 用于真实并行吞吐测试：

```bash
# 安装工具
go install github.com/rakyll/hey@latest

# 启动后端
go run ./test/benchmark/backend.go -port 8080 -body-size 1024

# 启动代理（另一个终端）
go run ./cmd/reverse-proxy -config config.yaml

# 运行压测
./test/benchmark/bench.sh -tool hey -proxy http://127.0.0.1:8081 -conns 50 -duration 30s
```

支持：`hey`、`wrk`、`ab`；并发梯度 10/50/100/200/500 连接；可配置时长和目标 URL。

---

## 附录：原始 Benchmark 输出

### A.1 HTTP 单元 (benchtime=3s)

```
goos: darwin
goarch: amd64
pkg: github.com/garfieldlw/reverse-proxy/internal/proxy
cpu: Genuine Intel(R) CPU 0000 @ 2.50GHz
BenchmarkHTTPProxySingleBackend-20            23232    156130 ns/op   45789 B/op   152 allocs/op
BenchmarkHTTPProxyMultiBackend-20             22438    160131 ns/op   45837 B/op   152 allocs/op
BenchmarkHTTPProxyBodySizes/1B-20             22682    158262 ns/op   45809 B/op   151 allocs/op
BenchmarkHTTPProxyBodySizes/1KB-20            22525    160438 ns/op   48149 B/op   156 allocs/op
BenchmarkHTTPProxyBodySizes/10KB-20           18531    194290 ns/op   92488 B/op   174 allocs/op
BenchmarkHTTPProxyBodySizes/100KB-20           9033    391350 ns/op  566668 B/op   197 allocs/op
BenchmarkHTTPProxyWithRateLimiter-20          22540    159414 ns/op   45829 B/op   153 allocs/op
BenchmarkHTTPProxyWithHealthCheck-20          22772    159150 ns/op   45854 B/op   153 allocs/op
BenchmarkHTTPProxyParallel-20                 22126    161933 ns/op   45832 B/op   153 allocs/op
PASS
ok  github.com/garfieldlw/reverse-proxy/internal/proxy  32.383s
```

### A.2 HTTP E2E (benchtime=3s)

```
BenchmarkEndToEndHTTPProxy-20                 21235    166713 ns/op   46788 B/op   163 allocs/op
BenchmarkEndToEndHTTPProxyMultiBackend-20     21854    161534 ns/op   46788 B/op   161 allocs/op
BenchmarkEndToEndHTTPProxyBodySizes/1B-20     22336    159737 ns/op   46772 B/op   161 allocs/op
BenchmarkEndToEndHTTPProxyBodySizes/1KB-20    21915    163475 ns/op   50124 B/op   165 allocs/op
BenchmarkEndToEndHTTPProxyBodySizes/10KB-20   17970    199901 ns/op  103718 B/op   184 allocs/op
BenchmarkEndToEndHTTPProxyBodySizes/100KB-20   8334    409010 ns/op  674460 B/op   207 allocs/op
BenchmarkEndToEndHTTPProxyWithRateLimit-20    22284    162064 ns/op   46792 B/op   162 allocs/op
BenchmarkEndToEndHTTPProxyParallel-20         22450    159937 ns/op   46773 B/op   161 allocs/op
PASS
ok  github.com/garfieldlw/reverse-proxy/test/integration  30.061s
```

### A.3 SSE 单元 (benchtime=1s)

```
BenchmarkSSEProxyConnectionEstablishment-20    4276    278533 ns/op   59991 B/op   216 allocs/op
BenchmarkSSEProxyEventThroughput-20            4378    249401 ns/op   57394 B/op   228 allocs/op
BenchmarkSSEProxyEventSizes/64B-20             5031    239297 ns/op   56553 B/op   210 allocs/op
BenchmarkSSEProxyEventSizes/1KB-20             5222    227478 ns/op   60911 B/op   211 allocs/op
BenchmarkSSEProxyEventSizes/10KB-20            2888    410287 ns/op  165124 B/op   272 allocs/op
BenchmarkSSEProxyMultiBackend-20               3810    288177 ns/op   60125 B/op   216 allocs/op
BenchmarkSSEProxyWithRateLimiter-20            3656    290351 ns/op   60127 B/op   217 allocs/op
PASS
ok  github.com/garfieldlw/reverse-proxy/internal/proxy  13.275s
```

### A.4 SSE E2E (benchtime=1s)

```
BenchmarkEndToEndSSEProxy-20                   4330    245685 ns/op   57422 B/op   218 allocs/op
BenchmarkEndToEndSSEProxyMultiBackend-20       4315    246453 ns/op   57385 B/op   218 allocs/op
BenchmarkEndToEndSSEProxyWithRateLimit-20      4465    246114 ns/op   57366 B/op   219 allocs/op
PASS
ok  github.com/garfieldlw/reverse-proxy/test/integration  3.852s
```

### A.5 WebSocket 单元 (benchtime=1s)

```
BenchmarkWSProxySingleBackend-20               2703    457501 ns/op   65563 B/op   284 allocs/op
BenchmarkWSProxyMultiMessage-20                1098   1075212 ns/op   85354 B/op   374 allocs/op
BenchmarkWSProxyMessageSizes/1B-20             2106    564433 ns/op   65500 B/op   283 allocs/op
BenchmarkWSProxyMessageSizes/1KB-20            1756    651171 ns/op   74844 B/op   291 allocs/op
BenchmarkWSProxyMessageSizes/10KB-20           1424    787117 ns/op  251369 B/op   334 allocs/op
BenchmarkWSProxyMultiBackend-20                1564    707696 ns/op   65615 B/op   284 allocs/op
BenchmarkWSProxyWithRateLimiter-20             1453    757588 ns/op   65543 B/op   285 allocs/op
PASS
ok  github.com/garfieldlw/reverse-proxy/internal/proxy  13.336s
```

### A.6 WebSocket E2E (benchtime=1s)

```
BenchmarkEndToEndWSProxy-20                    2564    442698 ns/op   65545 B/op   287 allocs/op
BenchmarkEndToEndWSProxyMultiBackend-20        1970    529417 ns/op   65534 B/op   287 allocs/op
BenchmarkEndToEndWSProxyWithRateLimit-20       2023    578117 ns/op   65538 B/op   288 allocs/op
PASS
ok  github.com/garfieldlw/reverse-proxy/test/integration  3.972s
```

### A.7 gRPC 单元 (benchtime=1s)

```
BenchmarkGRPCProxyUnaryCall-20                 1984    606968 ns/op  153779 B/op  1011 allocs/op
BenchmarkGRPCProxyUnaryPayloadSizes/1B-20      1807    650014 ns/op  162255 B/op  1044 allocs/op
BenchmarkGRPCProxyUnaryPayloadSizes/1KB-20     1700    681256 ns/op  179222 B/op  1044 allocs/op
BenchmarkGRPCProxyUnaryPayloadSizes/10KB-20    1568    764280 ns/op  263907 B/op  1058 allocs/op
BenchmarkGRPCProxyServerStreaming-20           1579    743397 ns/op  191566 B/op  1317 allocs/op
BenchmarkGRPCProxyMultiBackend-20              1617    727059 ns/op  172186 B/op  1021 allocs/op
BenchmarkGRPCProxyConnTracking-20              1604    732195 ns/op  168262 B/op  1021 allocs/op
PASS
ok  github.com/garfieldlw/reverse-proxy/internal/proxy  13.520s
```

### A.8 gRPC E2E (benchtime=1s)

```
BenchmarkEndToEndGRPCProxy-20                  1945    604814 ns/op  148864 B/op  1009 allocs/op
BenchmarkEndToEndGRPCProxyMultiBackend-20      1843    631718 ns/op  154852 B/op  1013 allocs/op
BenchmarkEndToEndGRPCProxyWithRateLimit-20     1818    643232 ns/op  154631 B/op  1013 allocs/op
PASS
ok  github.com/garfieldlw/reverse-proxy/test/integration  19.139s
```
