**笔记**

重要概念
- 标签（Label）：一个 <string, string> 的 KV
- 序列（Series）：多个 Label 的集合，{k1: v1, k2: v2, …}
- 数据块（Chunk）：TSDB 在磁盘中存储的 <T, V> 数据
- Index：TSDB 用于定位 Label 和 Series 以及对应的 <T, V> 时间系列的位置
- meta.json：人类可读的用于了解一个 Chunk 包含的数据范围的描述文件

一个series独占一个文件
- 文件非常多，容易达到进程最大打开文件数
- 每次持久化的文件非常多
- 删除的过期文件也非常多
- 最近的数据查询频率高
> 时序场景下，数据的查询、删除与时间密切相关，与series维度关联度不强

按时间分块存储
- 按需读取指定时间范围内的文件
- 删除指定的文件即可清理过期数据

目录结构
```
./data
├── b-000001
│   ├── chunks
│   │   ├── 000001
│   │   ├── 000002
│   │   └── 000003
│   ├── index
│   └── meta.json
```
data下一级目录是以b-为前缀，自增id为后缀的目录，代表block，一个block包含
- chunks：存放chunk数据，一个chunk包含很多序列
- index：对chunk的索引，快速定位序列以及数据所在的文件
- meta.json：描述block数据和状态的文件

设计思想
- 按照时间维度切分成多个block
- 每个block都是一个独立的数据库
- block之间完全没有交叉
- 每个block的文件都不可修改
- 只有最近的block允许接收数据，接收的数据先写入内存，同时也会写一份WAL
> 完全借鉴了LSM思想，并针对时序场景做了一些优化

label查询
- label-series倒排索引
- seriesID-series正排索引
- seriesID有序集合求交集/并集

WAL
- 数据目录是只读的，只有两种写情况：创建、压缩
- 最新数据存放在内存中
- 通过WAL保证持久性

源码目录结构
- chunknc
  - 提供时序点数据的编码格式，定义了Chunk接口，包含Appender、Iterator方法，还给出了Chunk的实现XORChunk
  - Chunk为一组数据点（t，v）的集合，通过Appender写入，通过Iterator遍历











mmap
- mmap对文件对读写只需要从磁盘到用户主存一次拷贝，跳过了内核空间
- 操作mmap映射后的文件，就像操作内存一样
- mmap会消耗虚拟内存，所以文件过大时，可以考虑只映射需要的那部分