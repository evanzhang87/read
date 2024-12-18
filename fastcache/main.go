package main

import (
	"fmt"
	"github.com/VictoriaMetrics/fastcache"
)

func main() {
	cache := fastcache.New(1024 * 1024)
	value := "aaaabbbbccccddddaaaabbbbccccddddaaaabbbbccccddddaaaabbbbccccdddd"
	fmt.Println(len([]byte(value)))
	for i := 0; i < 102400; i++ {
		key := fmt.Sprintf("key%d", i)
		cache.Set([]byte(key), []byte(value))
	}

	miss := 0
	for i := 0; i < 102400; i++ {
		key := fmt.Sprintf("key%d", i)
		var dst []byte
		_, ok := cache.HasGet(dst, []byte(key))
		if !ok {
			miss += 1
		}
	}
	// miss 0
	fmt.Println("miss", miss)

	for i := 0; i < 1024000; i++ {
		key := fmt.Sprintf("key%d", i)
		cache.Set([]byte(key), []byte(value))
	}

	miss = 0
	for i := 0; i < 1024000; i++ {
		key := fmt.Sprintf("key%d", i)
		var dst []byte
		_, ok := cache.HasGet(dst, []byte(key))
		if !ok {
			miss += 1
		}
	}
	// miss 588800
	fmt.Println("miss", miss)
}
