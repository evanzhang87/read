## Jsonpath
##### 这里实际指的是 https://github.com/childe/gohangout 的json相关的用法，不是广义的jsonpath

如何设计一个 jsonpath:  
给一串json，要求修改对应字段的值. 
```
{
  "@timestamp": "2025-11-17T09:07:05.092397906Z",
  "host": "HOSTNAME",
  "metadata": {
    "data": {
      "content": "test value"
    }
  },
  "vector": {
    "component_id": "win_parser",
    "component_kind": "transform",
    "component_type": "remap"
  }
}
```
假设要修改`metadata.data.content`，可以先分层，再修改
```
["@timestamp"]
[host]
[metadata]    [data]            [content]
[vector]      [component_id]
[vector]      [component_kind]
[vector]      [component_type]
```
```
// 根据肉眼观察直接可以写出这样的代码
func handler(event map[string]interface{}) {
	if inner, ok := event["metadata"]; ok {
		innerMap := inner.(map[string]interface{})
		if innerl2, ok2 := innerMap["data"]; ok2 {
			innerMapl2 := innerl2.(map[string]interface{})
			if innerl3, ok3 := innerMapl2["content"]; ok3 {
				fmt.Println(innerl3)
			}
		}
	}
}

// 然后发现有一个部分是可以复用的，即强制类型转换和查map，简单修改一下代码得到一个迭代
func handlerloop(event map[string]interface{}, field string) {
	if inner, ok := event[field]; ok {
		innerMap := inner.(map[string]interface{})
		handlerloop(innerMap, field)
	}
}

// 这里发现 field在实际的迭代过程中是需要变化的, metadata -> data -> content
// 那么将这些 string当成一个列表传进去，每次迭代的时候列表往后推一个即可（加一个index做标识也同理）
func handlerlooplist(event map[string]interface{}, fields []string) {
	if inner, ok := event[fields[0]]; ok {
		innerMap := inner.(map[string]interface{})
		handlerlooplist(innerMap, fields[1:])
	}
}

// 至此实际已经和 (fs *MultiLevelFieldSetter) SetField 的实现思路差不多了
// gohangout还多了两处优化: 
// 1. 对event中不存在的key有新增操作
// 2. 单独拆了一个lastField出来，这样可以少一次迭代，最后一层的field无论是否有嵌套都直接进行一次赋值
```
