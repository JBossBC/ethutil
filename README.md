# Concurrent_searchLog

concurrent_searchLog在go-ethereum中client.QueryFilter的基础上优化performance,主要优点如下:

+ 能够通过调用一个方法获取2k个以上的区块信息
+ 并发发送HTTP请求,性能为原本API的4倍左右
+ 使用控制反转优化因为频繁调用HTTP请求而出现的网络拥塞,极大降低了网络拥塞的可能性
+ 保证并发HTTP请求的原子性,在网络不会出现问题的情况下,一定能获取全部的日志


# logStream

## txFrom:查找由指定交易发起的交易

## FilterLog: 根据用户自己定义的过滤条件过滤指定的日志

      + func(log []types.Log, work *workUnit) error 用户需定义对局部日志的筛选函数

## search contract creator by logs (readying)
