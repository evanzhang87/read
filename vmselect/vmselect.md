# vmselect 查询执行与并发过程详解

> 以 `sum(sum_over_time(test.measurement_sum_value))` 和 `sum(sum_over_time(test.measurement_sum_value)) by (index)` 为例，全面分析从接收请求到返回结果的完整数据流转过程。

---

## 目录

1. [查询解析与执行入口](#1-查询解析与执行入口)
2. [阶段一：ProcessSearchQuery — 从 vmstorage 接收数据](#2-阶段一processsearchquery--从-vmstorage-接收数据)
    - [2.1 tmpBlocksFile：数据暂存层](#21-tmpblocksfile数据暂存层)
    - [2.2 tmpBlocksFileWrapper：索引层](#22-tmpblocksfilewrapper索引层)
    - [2.3 数据接收全流程](#23-数据接收全流程)
3. [阶段二：RunParallel — 按序列并行解包与计算](#3-阶段二runparallel--按序列并行解包与计算)
    - [3.1 第一层并行：timeseriesWork](#31-第一层并行timeserieswork)
    - [3.2 第二层并行：packedTimeseries.Unpack](#32-第二层并行packedtimeseriesunpack)
    - [3.3 mergeSortBlocks：归并排序与去重](#33-mergesortblocks归并排序与去重)
4. [阶段三：rollup 计算 — sum_over_time 滑动窗口](#4-阶段三rollup-计算--sum_over_time-滑动窗口)
5. [阶段四：增量聚合 — sum 的两阶段计算](#5-阶段四增量聚合--sum-的两阶段计算)
    - [5.1 sum(sum_over_time(...)) 无分组](#51-sumsum_over_time-无分组)
    - [5.2 sum(sum_over_time(...)) by (index) 有分组](#52-sumsum_over_time-by-index-有分组)
6. [完整并发模型总览](#6-完整并发模型总览)
7. [关键数据结构速查](#7-关键数据结构速查)

---

## 1. 查询解析与执行入口

### AST 结构

两条查询解析后的 AST：

```
# 查询1：sum(sum_over_time(test.measurement_sum_value))
AggrFuncExpr {
  Name:     "sum"
  Modifier: {}                    ← 无 by/without，所有序列合并为1条
  Args: [
    FuncExpr {
      Name: "sum_over_time"
      Args: [MetricExpr("test.measurement_sum_value")]
    }
  ]
}

# 查询2：sum(sum_over_time(test.measurement_sum_value)) by (index)
AggrFuncExpr {
  Name:     "sum"
  Modifier: {Op:"by", Args:["index"]}   ← 按 index 标签分组
  Args: [
    FuncExpr {
      Name: "sum_over_time"
      ...
    }
  ]
}
```

### evalExpr 路径选择

`evalExpr` 在 `eval.go` 中判断走哪条路径：

```go
if ae, ok := e.(*metricsql.AggrFuncExpr); ok {
    if callbacks := getIncrementalAggrFuncCallbacks(ae.Name); callbacks != nil {
        // sum 支持增量聚合，尝试优化路径
        fe, nrf := tryGetArgRollupFuncWithMetricExpr(ae)
        if fe != nil {
            // 内层是 sum_over_time(MetricExpr)，走增量聚合优化路径
            // （本文重点分析此路径）
            iafc := newIncrementalAggrFuncContext(ae, callbacks)
            return evalRollupFunc(ec, fe.Name, rf, e, re, iafc)
        }
    }
}
```

**`sum` 支持增量聚合的意义**：不需要先把所有序列的 rollup 结果收集齐再聚合，而是边解包边聚合，大幅节省内存。

---

## 2. 阶段一：ProcessSearchQuery — 从 vmstorage 接收数据

### 2.1 tmpBlocksFile：数据暂存层

`tmpBlocksFile` 是一个**逻辑连续的字节缓冲**，优先用内存，内存不够时溢出到临时文件：

```
内存阶段（tbf.f == nil）：
  tbf.buf = [block_A1_bytes | block_B1_bytes | block_A2_bytes | block_C1_bytes | ...]
             ↑ 所有序列的 block 混合存储，按到达顺序追加

磁盘溢出阶段（tbf.f != nil）：
  /tmp/searchResults/xxxxx 文件，内容与 buf 等价
  tbf.r = ReaderAt（支持随机读）
```

#### buf 中每个 block 的格式

每次 `WriteBlockData` 写入的不是原始数据，而是经过压缩编码的紧凑格式：

```
┌───────────────────────────────────────────┐
│ blockHeader（序列化）                      │
│   MinTimestamp, MaxTimestamp              │
│   FirstValue, RowsCount, Scale            │
│   TimestampsMarshalType, ValuesMarshalType│
│   TimestampsBlockSize, ValuesBlockSize    │
├───────────────────────────────────────────┤
│ len(4字节) + timestampsData               │
│   = delta编码 + zigzag + 可选压缩          │
├───────────────────────────────────────────┤
│ len(4字节) + valuesData                   │
│   = delta编码 + zigzag + 可选压缩          │
└───────────────────────────────────────────┘
```

> **设计目标**：以压缩形式暂存，推迟到真正计算时才解压，减少峰值内存。

#### tmpBlockAddr：逻辑地址

每次写入都分配一个地址，无论数据在内存还是磁盘：

```go
func (tbf *tmpBlocksFile) WriteBlockData(b []byte) (tmpBlockAddr, error) {
    addr.offset = tbf.offset   // 写入前的全局偏移量
    addr.size = len(b)
    tbf.offset += uint64(addr.size)  // 推进偏移，与是否落盘无关
    // ...
}
```

读取时根据 `tbf.f == nil` 判断从内存切片还是文件 pread：

```go
func (tbf *tmpBlocksFile) MustReadBlockAt(dst *Block, addr tmpBlockAddr) {
    if tbf.f == nil {
        buf = tbf.buf[addr.offset : addr.offset+uint64(addr.size)] // 零拷贝
    } else {
        tbf.r.MustReadAt(bb.B, int64(addr.offset))                 // pread
    }
}
```

---

### 2.2 tmpBlocksFileWrapper：索引层

`tmpBlocksFileWrapper` 是本次查询的"收件箱"，管理 block 到序列的映射关系：

```go
type tmpBlocksFileWrapper struct {
    mu                 sync.Mutex           // 并发写入保护
    tbf                *tmpBlocksFile       // 底层存储
    m                  map[string][]tmpBlockAddr  // key=序列化的MetricName, value=该序列所有block地址
    orderedMetricNames []string             // 记录序列第一次出现的顺序
}
```

#### m 的 key 和 value

| 字段 | 类型 | 含义 | 示例 |
|------|------|------|------|
| key | `string` | 序列化的 MetricName 字节串 | `"\x02index\x010\x04host\x02a1"` |
| value | `[]tmpBlockAddr` | 该序列所有 block 在 tbf 中的地址 | `[{0,300}, {800,500}, {2100,400}]` |

**一条序列会有多个 addr 的原因**：
- vmstorage 按时间分段存储，一条序列可能跨多个 block（时间范围长）
- 多副本场景（`replicationFactor>1`）下，多个 vmstorage 节点都会返回同一序列的 block

##### 具体示例

```
假设查询时间范围 [0, 180s]，3个 vmstorage 节点，replicationFactor=2

收到的 MetricBlock 流（按到达顺序）：
  node0: {index="0"} block[0-60s]    → addr={offset:0,    size:300}
  node0: {index="1"} block[0-60s]    → addr={offset:300,  size:500}
  node1: {index="0"} block[0-60s]    → addr={offset:800,  size:300}  ← 副本
  node0: {index="0"} block[60-120s]  → addr={offset:1100, size:350}
  node1: {index="1"} block[0-60s]    → addr={offset:1450, size:500}  ← 副本
  node0: {index="0"} block[120-180s] → addr={offset:1950, size:320}
  ...

最终 m:
  "{index=0}" → [{0,300}, {800,300}, {1100,350}, {1950,320}]
                   ↑node0   ↑node1副本  ↑node0       ↑node0
  "{index=1}" → [{300,500}, {1450,500}]
                   ↑node0     ↑node1副本

orderedMetricNames = ["{index=0}", "{index=1}"]  ← 保持插入顺序
```

#### orderedMetricNames 的作用

Go 的 map 遍历是随机的，`orderedMetricNames` 保证了后续构造 `packedTimeseries` 的顺序确定性。同时它与 `m` 的 key 共享同一份 `string` 内存（优化），避免大量序列名的重复分配。

#### RegisterAndWriteBlock 全流程

```go
func (tbfw *tmpBlocksFileWrapper) RegisterAndWriteBlock(mb *storage.MetricBlock) error {
    bb.B = storage.MarshalBlock(bb.B[:0], &mb.Block)  // 1. 序列化压缩
    tbfw.mu.Lock()
    addr, err := tbfw.tbf.WriteBlockData(bb.B)         // 2. 写入 tbf，得到地址
    // 3. 地址记录到 m[metricName]
    addrs := tbfw.m[string(metricName)]
    if len(addrs) == 0 {
        // 首次见到此序列：记录到 orderedMetricNames，key 复用同一字符串内存
        tbfw.orderedMetricNames = append(tbfw.orderedMetricNames, string(metricName))
        tbfw.m[tbfw.orderedMetricNames[len-1]] = append(addrs, addr)
    } else {
        tbfw.m[string(metricName)] = append(addrs, addr)
    }
    tbfw.mu.Unlock()
}
```

---

### 2.3 数据接收全流程

```
ProcessSearchQuery
│
├─ 创建 tbfw（本次查询专用，不跨查询共享）
│
├─ processSearchQuery（内部对每个 vmstorage node 起一个 goroutine）
│   ├─ goroutine_node0: processSearchQueryOnConn
│   │     循环读 TCP：每收到一个 MetricBlock 立即回调 processBlock
│   │       → tbfw.RegisterAndWriteBlock(mb)   ← 加 mutex 写入
│   ├─ goroutine_node1: 同上
│   └─ goroutine_node2: 同上
│       ↑ 所有节点并行接收，processBlock 靠 mutex 串行写入同一个 tbf
│
├─ tbf.Finalize()
│   若有溢出文件：把 buf 剩余写入文件，开 ReaderAt
│
└─ 构造 Results
    rss.tbf = tbfw.tbf    ← tbf 所有权转移
    rss.packedTimeseries = [
      {metricName:"{index=0}", addrs:[{0,300},{800,300},{1100,350},{1950,320}]},
      {metricName:"{index=1}", addrs:[{300,500},{1450,500}]},
    ]
    tbfw 本身被 GC 回收（m 和 orderedMetricNames 数据已转移到 packedTimeseries）
```

---

## 3. 阶段二：RunParallel — 按序列并行解包与计算

### 3.1 第一层并行：timeseriesWork

**并发粒度：序列级**。每条 `packedTimeseries` 是一个独立工作单元。

```
workers = min(len(packedTimeseries), GOMAXPROCS)

workChs[0] = make(chan *timeseriesWork, 16) → goroutine worker0
workChs[1] = make(chan *timeseriesWork, 16) → goroutine worker1
...

# 分发：每条序列创建一个 tsw，随机放入某个 workCh
tsw_A → workChs[rand] → worker0
tsw_B → workChs[rand] → worker1
tsw_C → workChs[rand] → worker0
...
```

#### timeseriesWork 结构

```go
type timeseriesWork struct {
    mustStop *uint32          // 所有 tsw 共享，原子量，任一出错则全部停止
    rss      *Results         // 指向共享的 Results（读 tbf 和元信息）
    pts      *packedTimeseries // 本任务负责的序列
    f        func(rs *Result, workerID uint) error  // 上层注入的计算回调
    doneCh   chan error        // 缓冲1，完成通知
}
```

#### worker 执行逻辑：timeseriesWorker

每个 worker goroutine **复用一个 `Result` 对象**（从 `resultPool` 取），处理完一条序列后立即复用给下一条：

```go
func timeseriesWorker(ch <-chan *timeseriesWork, workerID uint) {
    r := resultPool.Get().(*result)  // 从 pool 取，避免每条序列都分配
    for tsw := range ch {
        tsw.do(&r.rs, workerID)
        tsw.doneCh <- err
    }
    resultPool.Put(r)
}
```

#### tsw.do() 执行序列

```go
func (tsw *timeseriesWork) do(r *Result, workerID uint) error {
    // 1. 快速失败：检查全局停止标志
    if atomic.LoadUint32(tsw.mustStop) != 0 { return nil }

    // 2. 超时检查
    if rss.deadline.Exceeded() {
        atomic.StoreUint32(tsw.mustStop, 1)
        return error
    }

    // 3. 核心：解包这条序列的所有 block
    tsw.pts.Unpack(r, rss.tbf, rss.tr, ...)
    //  → r.Timestamps = [10000, 20000, 30000, ...]  解压后的时间戳
    //  → r.Values     = [1.0, 2.0, 3.0, ...]        解压后的 float64 值

    // 4. 执行上层回调（rollup + 聚合）
    if len(r.Timestamps) > 0 {
        tsw.f(r, workerID)
    }
}
```

#### 主线程等待与汇总

```go
// 主线程按原始顺序等待每条序列的完成信号
for _, tsw := range tsws {
    err := <-tsw.doneCh  // 阻塞等待该序列处理完
    rowsProcessedTotal += tsw.rowsProcessed
    putTimeseriesWork(tsw)
}
// 关闭所有 workCh，worker goroutine 优雅退出
for _, workCh := range workChs { close(workCh) }
```

> **为什么用本地 worker 而不是全局 goroutine pool？**
> 防死锁：若使用全局 pool，嵌套调用 `RunParallel` 时，内层调用会耗尽 pool 中所有 worker，导致外层调用的 work 永远无法被处理，形成死锁。

---

### 3.2 第二层并行：packedTimeseries.Unpack

每条序列可能有几百上千个 block，这里**再开一层并行**，按批次解压：

```
pts.addrs = [addr0, addr1, ..., addr9999]  ← 假设 10000 个 block

按 unpackBatchSize=5000 分批：
  batch0: [addr0..addr4999]   → unpackWork0 → unpackWorker goroutine
  batch1: [addr5000..addr9999] → unpackWork1 → unpackWorker goroutine

每个 unpackWorker 对 batch 中每个 addr：
  1. tbf.MustReadBlockAt(tmpBlock, addr)   ← 读压缩字节
  2. tmpBlock.UnmarshalData()              ← 解压
  3. AppendRowsWithTimeRangeFilter(tr)     ← 时间范围过滤
  → 得到 sortBlock{Timestamps []int64, Values []float64}
```

> **unpackBatchSize = 5000 的由来**：单 goroutine 解压速度约 40M rows/s，单 block 最多 8K rows，所以 40M/8K ≈ 5000，让一个 goroutine 约工作 1 秒，减少跨 CPU 内存 ping-pong 的频率。

> **内层 channel 为无缓冲**：大多数序列只有 1 个 batch，无缓冲能减少 CPU 间的内存拷贝开销。

---

### 3.3 mergeSortBlocks：归并排序与去重

多个 `sortBlock` 各自内部有序，但相互之间可能时间交叉（不同时间段的 block）或重复（副本）：

```
来自 vmstorage_node0 的 block：
  sortBlock0: Timestamps=[10000, 30000, 60000] Values=[1.0, 2.0, 3.0]

来自 vmstorage_node1 的 block（时间交叉）：
  sortBlock1: Timestamps=[20000, 50000, 90000] Values=[5.0, 6.0, 7.0]

来自 vmstorage_node2 的 block（副本，与 node0 重复）：
  sortBlock2: Timestamps=[10000, 30000, 60000] Values=[1.0, 2.0, 3.0]

↓ heap.Init（最小堆，按 block 当前队首时间戳排序）
↓ 逐步 Pop 最小值追加

归并后: t=[10000,10000,20000,30000,30000,50000,60000,60000,90000]
        v=[1.0,  1.0,  5.0,  2.0,  2.0,  6.0,  3.0,  3.0,  7.0]

↓ DeduplicateSamples（相同时间戳保留一个）

最终: t=[10000, 20000, 30000, 50000, 60000, 90000]
      v=[1.0,   5.0,   2.0,   6.0,   3.0,   7.0]
```

至此，`Result{Timestamps, Values}` 就是这条序列**全时间范围内所有样本**，已排序、已去重，可以直接交给 rollup 函数处理。

---

## 4. 阶段三：rollup 计算 — sum_over_time 滑动窗口

**发生位置**：worker goroutine 内，每条序列独立计算，完全并行。

**关键代码**：`rollupConfig.doInternal()` in `rollup.go`。

### 输入假设

```
查询参数：start=60s, end=180s, step=60s, window=60s
sharedTimestamps = [60000ms, 120000ms, 180000ms]

序列 {index="0"} 的原始样本（Unpack 后）：
  Timestamps = [10000, 30000, 60000, 90000, 120000, 150000, 180000]
  Values     = [1.0,   2.0,   3.0,   4.0,   5.0,    6.0,    7.0]

序列 {index="1"} 的原始样本（Unpack 后）：
  Timestamps = [20000, 60000, 100000, 140000, 180000]
  Values     = [10.0,  20.0,  30.0,   40.0,   50.0]
```

### 窗口语义

每个时间步 `tEnd` 对应的窗口是 **(tEnd - window, tEnd]**（左开右闭）：

```
tEnd=60000:  窗口 = (0, 60000]     → 包含 t=10000,30000,60000
tEnd=120000: 窗口 = (60000, 120000] → 包含 t=90000,120000
tEnd=180000: 窗口 = (120000, 180000] → 包含 t=150000,180000
```

### 核心循环（doInternal）

```go
i, j := 0, 0  // 双指针，i=窗口左边界后第一个，j=窗口右边界后第一个
for _, tEnd := range rc.Timestamps {  // [60000, 120000, 180000]
    tStart := tEnd - window

    // 移动 i：跳过 timestamps[i] <= tStart 的点（左开）
    ni = seekFirstTimestampIdxAfter(timestamps[i:], tStart, ni)
    i += ni

    // 移动 j：跳过 timestamps[j] <= tEnd 的点（右闭，所以找第一个 > tEnd 的）
    nj = seekFirstTimestampIdxAfter(timestamps[j:], tEnd, nj)
    j += nj

    rfa.values = values[i:j]  // 窗口内的值
    value := rollupSum(rfa)   // 求和
    dstValues = append(dstValues, value)
}
```

### 序列 {index="0"} 的逐步计算

```
步骤1：tEnd=60000, tStart=0, 窗口=(0, 60000]
  i=0（10000 > 0，不跳过）
  j=3（90000 > 60000，所以 j=3）
  rfa.values = values[0:3] = [1.0, 2.0, 3.0]
  rollupSum  = 1+2+3 = 6.0

步骤2：tEnd=120000, tStart=60000, 窗口=(60000, 120000]
  i=3（seekFirstTimestampIdxAfter: 60000不>60000, 90000>60000 → i=3）
  j=5（150000>120000 → j=5）
  rfa.values = values[3:5] = [4.0, 5.0]
  rollupSum  = 4+5 = 9.0

步骤3：tEnd=180000, tStart=120000, 窗口=(120000, 180000]
  i=5（150000>120000 → i=5）
  j=7（全部结束 → j=7）
  rfa.values = values[5:7] = [6.0, 7.0]
  rollupSum  = 6+7 = 13.0

{index="0"} sum_over_time 结果：
  Timestamps = [60000, 120000, 180000]
  Values     = [6.0,   9.0,   13.0]
```

### 序列 {index="1"} 的逐步计算

```
步骤1：tEnd=60000, 窗口=(0, 60000]
  timestamps=[20000, 60000, 100000, 140000, 180000]
  i=0（20000>0），j=2（100000>60000）
  rfa.values = [10.0, 20.0]
  rollupSum  = 10+20 = 30.0

步骤2：tEnd=120000, 窗口=(60000, 120000]
  i=2（100000>60000），j=3（140000>120000）
  rfa.values = [30.0]
  rollupSum  = 30.0

步骤3：tEnd=180000, 窗口=(120000, 180000]
  i=3（140000>120000），j=5（到末尾）
  rfa.values = [40.0, 50.0]
  rollupSum  = 40+50 = 90.0

{index="1"} sum_over_time 结果：
  Timestamps = [60000, 120000, 180000]
  Values     = [30.0,  30.0,   90.0]
```

---

## 5. 阶段四：增量聚合 — sum 的两阶段计算

**增量聚合（Incremental Aggregate）** 的核心思想：不等所有序列的 rollup 结果都生成完再聚合，而是每处理完一条序列就立刻累加进共享结构，节省内存。

### incrementalAggrContext 结构

```go
type incrementalAggrContext struct {
    ts     *timeseries  // 累积值：ts.Values[i] 存第 i 步的当前 sum
    values []float64    // 计数器：values[i] 存第 i 步已累加了几条序列（0表示还没有）
}
```

**为什么需要 `values` 计数器？**
初始时 `ts.Values = [0, 0, 0]`（Go 零值），第一次累加时不能做 `0 + v`（会把初始零计入 sum），而是直接赋值 `= v`；`values[i] == 0` 就是这个区分的标志。

### updateAggrSum 逻辑

```go
func updateAggrSum(iac *incrementalAggrContext, values []float64) {
    for i, v := range values {
        if math.IsNaN(v) { continue }           // 跳过 NaN
        if dstCounts[i] == 0 {
            dstValues[i] = v                    // 第一条序列：直接赋值
            dstCounts[i] = 1
        } else {
            dstValues[i] += v                   // 后续序列：累加
        }
    }
}
```

### 5.1 sum(sum_over_time(...)) 无分组

`removeGroupTags(modifier={})` → `RemoveTagsOn([])` → **清除所有标签**

所有序列的标签都被清空，`marshalMetricNameSorted({})` → key = `""`，全部归入同一个 `iac`。

#### Worker 阶段（并行）：按 workerID 分桶，无锁

```
worker0 处理 {index="0"}，values=[6.0, 9.0, 13.0]：
  key = "" → iafc.m[worker0][""] 不存在，创建 iac0
  iac0.ts.Values  = [0, 0, 0]    初始
  iac0.values     = [0, 0, 0]    计数器初始
  updateAggrSum(iac0, [6.0, 9.0, 13.0]):
    i=0: count=0 → dstValues[0]=6.0,  count=1
    i=1: count=0 → dstValues[1]=9.0,  count=1
    i=2: count=0 → dstValues[2]=13.0, count=1
  iac0 状态：Values=[6.0, 9.0, 13.0], counts=[1,1,1]

worker1 处理 {index="1"}，values=[30.0, 30.0, 90.0]：
  key = "" → iafc.m[worker1][""] 不存在，创建 iac1（worker1 独立分桶，无冲突）
  updateAggrSum(iac1, [30.0, 30.0, 90.0]):
    i=0: count=0 → dstValues[0]=30.0, count=1
    ...
  iac1 状态：Values=[30.0, 30.0, 90.0], counts=[1,1,1]
```

#### 主线程 finalizeTimeseries（串行）：跨 worker 归并

```
mGlobal = {}
遍历 iafc.m[worker0]:
  key="" → mGlobal[""] = iac0        （首次见到 key，直接存入）

遍历 iafc.m[worker1]:
  key="" → mGlobal[""] 已存在
  mergeAggrSum(mGlobal[""], iac1):
    srcCounts=[1,1,1], dstCounts=[1,1,1]
    i=0: srcCount=1, dstCount=1 → dstValues[0] += 30.0 → 6.0+30.0 = 36.0
    i=1: → 9.0+30.0 = 39.0
    i=2: → 13.0+90.0 = 103.0

finalizeAggrCommon：
  遍历 counts，若 counts[i]==0 则 dstValues[i]=NaN
  counts=[1,1,1] 全非0，值不变

最终输出（1条时序）：
  {}  →  Values=[36.0, 39.0, 103.0]  对应时间步 [60s, 120s, 180s]
```

---

### 5.2 sum(sum_over_time(...)) by (index) 有分组

`removeGroupTags(modifier={by,["index"]})` → `RemoveTagsOn(["index"])` → **只保留 index 标签**

不同 `index` 值的序列映射到不同的 key，各自独立累加。

#### Worker 阶段（并行）

```
worker0 处理 {index="0", host="a"}，经 removeGroupTags 后：
  MetricName 只剩 {index="0"}
  key = marshalMetricNameSorted({index="0"}) = "k1"
  → iafc.m[worker0]["k1"] = iac_index0
  updateAggrSum(iac_index0, [6.0, 9.0, 13.0])
  状态：Values=[6.0, 9.0, 13.0], counts=[1,1,1]

worker1 处理 {index="1", host="b"}，经 removeGroupTags 后：
  MetricName 只剩 {index="1"}
  key = "k2"（不同于 k1）
  → iafc.m[worker1]["k2"] = iac_index1
  updateAggrSum(iac_index1, [30.0, 30.0, 90.0])
  状态：Values=[30.0, 30.0, 90.0], counts=[1,1,1]
```

#### 主线程 finalizeTimeseries（串行）

```
mGlobal = {}
遍历 iafc.m[worker0]:
  "k1" → mGlobal["k1"] = iac_index0

遍历 iafc.m[worker1]:
  "k2" → mGlobal["k2"] = iac_index1  （key 不同，不合并）

finalizeAggrCommon：counts 全非0，值不变

最终输出（2条时序）：
  {index="0"} → Values=[6.0,  9.0, 13.0]
  {index="1"} → Values=[30.0, 30.0, 90.0]
```

---

## 6. 完整并发模型总览

```
┌─────────────────────────────────────────────────────────────────────────┐
│ 阶段一：ProcessSearchQuery（I/O 并行）                                   │
│                                                                          │
│  goroutine_node0    goroutine_node1    goroutine_node2                  │
│       │                  │                  │                            │
│   vmstorage0          vmstorage1         vmstorage2                     │
│       │  MetricBlock     │  MetricBlock     │  MetricBlock               │
│       └──────────────────┴──────────────────┘                           │
│                    processBlock(mb) 回调（流式，每收到一个立即处理）      │
│                    RegisterAndWriteBlock → 加 mutex → 写入 tbf            │
└─────────────────────────────────────────────────────────────────────────┘
                              ↓ 构造 Results{tbf, packedTimeseries}
┌─────────────────────────────────────────────────────────────────────────┐
│ 阶段二+三+四：RunParallel（CPU 并行，序列级）                             │
│                                                                          │
│  worker goroutine 0           worker goroutine 1                        │
│  从 workCh[0] 取任务           从 workCh[1] 取任务                       │
│  ├─ tsw(序列A).do()           ├─ tsw(序列B).do()                        │
│  │    Unpack(序列A)            │    Unpack(序列B)                        │
│  │    ├─ unpackWorker: batch0  │    ├─ unpackWorker: batch0              │
│  │    └─ unpackWorker: batch1  │    └─ ...                               │
│  │    mergeSortBlocks          │    mergeSortBlocks                      │
│  │    → Result{T,V}            │    → Result{T,V}                        │
│  │                             │                                         │
│  │    rollupSum（滑窗）         │    rollupSum（滑窗）                     │
│  │    → ts.Values=[6,9,13]     │    → ts.Values=[30,30,90]              │
│  │                             │                                         │
│  │    updateAggrSum            │    updateAggrSum                        │
│  │    → iafc.m[0][key]+=ts     │    → iafc.m[1][key]+=ts  ← 无锁分桶    │
│  │                             │                                         │
│  └─ tsw(序列C).do()            └─ tsw(序列D).do()                        │
│       ...                           ...                                  │
└─────────────────────────────────────────────────────────────────────────┘
                              ↓ RunParallel 返回后
┌─────────────────────────────────────────────────────────────────────────┐
│ 主线程串行：iafc.finalizeTimeseries()                                    │
│  mergeAggrSum 跨 worker 合并同一 key 的中间结果                          │
│  finalizeAggrCommon 将 count==0 的步骤置 NaN                             │
│  → 最终输出 []*timeseries                                                │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 7. 关键数据结构速查

### 数据形态变化链

```
vmstorage 响应
  MetricName: []byte（序列标签的序列化）
  Block.timestampsData: delta+zigzag 压缩字节
  Block.valuesData:     delta+zigzag 压缩字节
         ↓ MarshalBlock（重新打包，仍压缩）
tbf.buf 中一段字节
  = blockHeader + len+timestampsData + len+valuesData
         ↓ Unpack → UnmarshalBlock → UnmarshalData
sortBlock
  Timestamps: []int64   解压后的毫秒级时间戳
  Values:     []float64 解压后乘以 10^Scale 的浮点值
         ↓ mergeSortBlocks（归并+去重）
Result{Timestamps []int64, Values []float64}
  全时间范围，有序，无重复
         ↓ rollupConfig.doInternal（滑窗）
timeseries.Values []float64
  每个时间步的 rollup 结果（如 sum_over_time 的值）
         ↓ updateAggrSum（增量累加）
incrementalAggrContext.ts.Values []float64
  按分组 key 累加后的中间结果
         ↓ finalizeTimeseries（归并 + finalizeAggrCommon）
[]*timeseries
  最终查询结果
```

### 并发保护点

| 位置 | 保护机制 | 原因 |
|------|----------|------|
| `RegisterAndWriteBlock` | `tbfw.mu` Mutex | 多节点 goroutine 并发写入同一 tbf |
| `iafc.mLock` | Mutex | 初始化 `iafc.m[workerID]` 时的并发保护（map 增删） |
| `iafc.m[workerID]` 分桶 | 按 workerID 划分独立 map | 同一 worker 的序列访问自己分桶，无需锁 |
| `unpackWorker` channel | 无缓冲 | 减少 CPU 间内存 ping-pong |
| `timeseriesWorker` channel | 缓冲 16 | 允许调度器预填充，减少 goroutine 阻塞 |
| `mustStop uint32` | `atomic` 原子操作 | 无锁快速传播停止信号 |

### 关键参数

| 参数 | 默认值 | 含义 |
|------|--------|------|
| `unpackBatchSize` | 5000 blocks | 内层并行的最大批次大小 |
| `maxInmemoryTmpBlocksFile` | max(64KB, min(mem/1024, 4MB)) | tbf 内存上限，超出溢出到磁盘 |
| `gomaxprocs` | `cgroup.AvailableCPUs()` | 外层 RunParallel 的最大 worker 数 |
| `maxSamplesPerSeries` | 30e6 | 单序列最大样本数，防止 OOM |
| `maxSamplesPerQuery` | 1e9 | 单查询跨所有序列的最大样本数 |
| `workCh buffer` | 16 | 外层 timeseriesWork channel 缓冲大小 |
