## Atomic
##### 原子操作
原子操作是底层操作，通过cpu指令实现  
go 内存模型 https://go.dev/ref/mem  // TODO: read  
可以用`CompareAndSwap`来实现sync.once的操作  
看了下sync.once的源码，本质上也是原子操作
```
type Once struct {
	// done indicates whether the action has been performed.
	// It is first in the struct because it is used in the hot path.
	// The hot path is inlined at every call site.
	// Placing done first allows more compact instructions on some architectures (amd64/386),
	// and fewer instructions (to calculate offset) on other architectures.
	done atomic.Uint32
	m    Mutex
}
```
