## Fastcache
##### 特性: 无GC

数据结构:
```
type Cache struct {
	buckets [bucketsCount]bucket // 固定512个bucket

	bigStats BigStats
}

type bucket struct {
	mu sync.RWMutex

	// chunks is a ring buffer with encoded (k, v) pairs.
	// It consists of 64KB chunks.
	chunks [][]byte // 数据存储的地方

	// m maps hash(k) to idx of (k, v) pair in chunks.
	m map[uint64]uint64 // 索引存储的地方

	// idx points to chunks for writing the next (k, v) pair.
	idx uint64 // 下一个插入的索引

	// gen is the generation of chunks.
	gen uint64 // 迭代次数，用来判断数据过期

	getCalls    uint64
	setCalls    uint64
	misses      uint64
	collisions  uint64
	corruptions uint64
}
```

Set:
```
idx := h % bucketsCount
// 先对Key算Hash，决定放到哪个bucket

var kvLenBuf [4]byte
kvLenBuf[0] = byte(uint16(len(k)) >> 8)
kvLenBuf[1] = byte(len(k))
kvLenBuf[2] = byte(uint16(len(v)) >> 8)
kvLenBuf[3] = byte(len(v))
// 用4个字节来存放key/value的长度信息，这里假定了k/v的长度都小于16位，大于16位要用SetBig方法
// 2^16 = 2^10 * 2^6 = 64KB 

kvLen := uint64(len(kvLenBuf) + len(k) + len(v))
idx := b.idx
idxNew := idx + kvLen
// 预估一下当前chunk是否填满

chunk = append(chunk, kvLenBuf[:]...)
chunk = append(chunk, k...)
chunk = append(chunk, v...)
// 没填满的情况下直接把 kvlen/k/v 写到chunk里

b.m[h] = idx | (b.gen << bucketSizeBits)
// 更新索引
// 这里的value是一个uint64，其中高24位用来存储迭代次数，低40位用来存储插入的位置
// | 操作符是取或操作，idx | (b.gen << bucketSizeBits)直接进行了高位覆盖

缓存过期：
bGen := b.gen & ((1 << genSizeBits) - 1) // 本次迭代
bIdx := b.idx // 本次索引
bm := b.m // 索引缓存
// 遍历缓存
for _, v := range bm {
    gen := v >> bucketSizeBits // 从高24位获取迭代次数
    idx := v & ((1 << bucketSizeBits) - 1) // 高位清0获得索引
    if (gen+1 == bGen || gen == maxGen && bGen == 1) && idx >= bIdx || gen == bGen && idx < bIdx {
        newItems++
    }
}
// (gen+1 == bGen || gen == maxGen && bGen == 1) && idx >= bIdx 
// 为了让高位不丢失意义，gen永不为0，括号内判断gen为上一代，idx >= bIdx 判断环不被覆盖
// gen == bGen && idx < bIdx
// 在最新一代，并且环不被覆盖
// 符合条件的被标记成 newItems, 不会被过期

// 二次遍历
if newItems < len(bm) {
    bmNew := make(map[uint64]uint64, newItems) // 申请预留大小map，防止map自动扩容
    // 只保留newItems
    for k, v := range bm {
        gen := v >> bucketSizeBits
        idx := v & ((1 << bucketSizeBits) - 1)
        if (gen+1 == bGen || gen == maxGen && bGen == 1) && idx >= bIdx || gen == bGen && idx < bIdx {
            bmNew[k] = v
        }
    }
    b.m = bmNew // 其实这里还是有gc，旧的map会被gc掉
}
```

Get:
```
idx := h % bucketsCount
// 先对Key算Hash，决定去哪个bucket找

v := b.m[h] // 找索引

bGen := b.gen & ((1 << genSizeBits) - 1) // 预先算出当前的代数

gen := v >> bucketSizeBits
idx := v & ((1 << bucketSizeBits) - 1)
// 从索引中提取位信息，获得key的代数和位置

if gen == bGen && idx < b.idx || gen+1 == bGen && idx >= b.idx || gen == maxGen && bGen == 1 && idx >= b.idx 
// 判断是newItems，不会被过期

chunkIdx := idx / chunkSize 
chunk := chunks[chunkIdx]
// 先拿到chunk的序号，chunks [][]byte 的第一个[]

idx %= chunkSize // 在拿到索引的具体位置， chunks [][]byte 的第二个[]
kvLenBuf := chunk[idx : idx+4] // 前4位保留了k/v的的长度信息
keyLen := (uint64(kvLenBuf[0]) << 8) | uint64(kvLenBuf[1])
valLen := (uint64(kvLenBuf[2]) << 8) | uint64(kvLenBuf[3])
// 在往后读keyLen valLen的长度就能分别得到Key和Value

```
