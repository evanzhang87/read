## vmstorage中倒排索引的应用
##### vmstorage的indexdb中包含了多种索引，https://victoriametrics.com/blog/vmstorage-how-indexdb-works/ 这里说的是其中一种，即Tag->MetricID的映射

## 概念:
- Tag：这里的tag值的是kv键值对，比如`ip: 10.1.1.1`,`ip: 10.1.1.2`,`host: mymacbook` 都属于不同的tag
- MetricID: 每一条线都对应一个唯一的metricID，也就是说 指标名+所有的tag 完全一样才属于同一条线，反之则不是

## indexdb的存储和查询
##### 简化，这里直接当作行，实际是通过offset来区分每一段数据的开始和结束的
```
假设现在写入了三个指标 

measurementA{ip="10.1.1.1", host: "mymacbook"} 10001
measurementA{ip="10.1.1.2", host: "yourmymacbook"} 10002
measurementB{ip="10.1.1.1", host: "mymacbook"} 10003

其中 10001，10002，10003为各自的metricID
那么则会写入如下的索引，其中__name__作为指标名也被当作一个特殊tag写入
1 ip=10.1.1.1 10001,10003
1 ip=10.1.1.2 10002
1 host=mymacbook 10001,10003
1 host=yourmymacbook 10002
1 __name__=measurementA 10001,10002
1 __name__=measurementB 10003

这里的格式为 [1] [tag] [metricIDs], 其中[1]为索引类型，枚举值，Tag->MetricID索引这个值固定为1，[tag]是一组键值对，注意只有一组，[metricIDs]是一个列表，值的含义是包含了这个键值对的metricID列表
相信到这里你已经能猜到查询的过程里一定有一步查交集了。

假设我的查询语句为 measurementA{ip="10.1.1.1", host: "mymacbook"}
那么查询的步骤为: 
1. 拆分tags __name__="measurementA" ip="10.1.1.1" host: "mymacbook" 
2. 查询tags索引, 分别得到metricIDs列表 [ 10001,10002 ] [ 10001,10003 ] [ 10001,10003 ]
3. 求交集，得到metricIDs [ 10001 ]

假设我的查询语句直接为 measurementA{}
那么直接查出 metricIDs [ 10001,10002 ]

在得到得到metricIDs后，剩下的就是取值和计算环节了
```

## 倒排索引与正排索引索引对比的优势
```
在指标的一般场景下，一台机器会输出若干个指标，但是这些指标的tag很多都是一致的，比如机器属性相关的tag
measurementA{ip="10.1.1.1", host="mymacbook"}   10001
measurementA{ip="10.1.1.2", host="yourmacbook"} 10002
measurementB{ip="10.1.1.1", host="mymacbook"}   10003 
measurementB{ip="10.1.1.2", host="yourmacbook"} 10004
measurementC{ip="10.1.1.1", host="mymacbook"}   10005
measurementC{ip="10.1.1.2", host="yourmacbook"} 10006
...

1. 空间优势
在只有一级的正排索引情况下，索引存储结构就是上面的结构 
而倒排索引的结构则为
ip="10.1.1.1" 10001,10003,10005
ip="10.1.1.2" 10002,10004,10006
host="mymacbook" 10001,10003,10005
host="yourmacbook" 10002,10004,10006
__name__=measurementA 10001,10002
__name__=measurementB 10003,10004
__name__=measurementC 10005,10006

这里其实已经可以看出，倒排索引会比正排索引节省空间了

假设现在有1000个指标，每个指标有10个label，每个label有100种value，这里暂时忽略掉metrics value，这一部分属于索引值
那么在使用正排索引需要的空间近似计算为 
(1000*10*100) * (metric_len + 10 * (label_len + value_len)), 即所有的metricID * 单索引全长
是用倒排索引需要的空间近似计算为
(label_len + value_len) * (10 * 100) + 1000 * metric_len, 即所有的label kv枚举值 + 指标名
可以label越多，倒排索引的空间优势越大，因为省略了多次写重复label kv。

2. 多值查询优势
假设现在要查询 measurementA{ip="10.1.1.1", host="mymacbook"}
对于正向索引则为 
select value from metrics where name = "measurementA" and ip="10.1.1.1" and host="mymacbook"
对于这种经典的sql查询，查询速度取决于预先建立的索引数量，索引越多越快，没有索引只能全表查询

而倒排索引则是直接查询三个索引
__name__ = measurementA [10001,10002]
ip="10.1.1.1" [10001,10003,10005]
host="mymacbook" [10001,10003,10005]
然后取交集

二者对比可以看出，正向索引的优势在于预先创建索引用来对某些特定查询提速，而倒排索引则是相当于预先创建了若干小索引，对通用的查询提速
```
