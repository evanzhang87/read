package main

import (
	"fmt"
	"sync/atomic"
)

func main() {
	var once int32

	for i := 0; i < 3; i++ {
		fmt.Println(atomic.CompareAndSwapInt32(&once, 0, 1))
	}

	// true false false
}
