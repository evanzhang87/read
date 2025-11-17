package main

import (
	"encoding/json"
	"github.com/childe/gohangout/field_setter"
	"log"
	"os"
)

func main() {
	data, _ := os.ReadFile("./data.json")
	var event map[string]interface{}
	_ = json.Unmarshal(data, &event)
	printMap(event)

	onelevel := field_setter.NewOneLevelFieldSetter("host")
	onelevel.SetField(event, "newhostname", "no used", true) // 这里的 field实际没有作用
	printMap(event)

	// multilevel 替换多层的key
	// 假设现在有一个三层的json key: metadata.data.content
	// 初始化两个属性:
	// preFields: [metadata, data]
	// lastField: content
	multilevel := field_setter.NewMultiLevelFieldSetter([]string{"metadata", "data", "content"})

	// 这里有个迭代，对于多层的未知event来说，需要从第一层开始迭代匹配
	// for _, field := range fs.preFields // 这里其实有一个隐藏含义就是从最外层元素遍历
	// if value, ok := current[field]; ok // 如果有的话就迭代到里面一层
	// current = value.(map[string]any) // 从子map开始迭代
	// 1.
	//   "metadata": {
	//    "data": {
	//      "content": "value"
	//    },
	//    "kind": "event",
	//    "level": "ERROR",
	//    "module_path": "vector::internal_events::remap",
	//    "target": "vector::internal_events::remap"
	//  }
	//  2.
	//    "data": {
	//      "content": "value"
	//    },
	//  3. "content": "value" // 这里对content是最后一个元素，不在preFields列表中，在最后一行完成赋值
	// 	current[fs.lastField] = value
	//
	// 如果 第一层在event里面不存在: 比如 []string{"metadata1", "data", "content"}
	// 	current[field] = a // 先创建一个key为metadata1的空Map出来，
	//	current = a // 对空Map进行迭代，由于map为空，每次都会都走到这里，一直到走到最后一行
	//
	multilevel.SetField(event, "new message", "no used", true)
	printMap(event)

	multilevel = field_setter.NewMultiLevelFieldSetter([]string{"metadata1", "data", "content"})
	multilevel.SetField(event, "new message", "no used", true)
	printMap(event) // 这里会多一个 metadata1.data.content = newkind
}

func printMap(input interface{}) {
	bt, _ := json.Marshal(input)
	log.Println(string(bt))
}
