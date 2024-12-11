# weworkip

本仓库提供一种企微自建应用saas(包括会话存档)解决方案

利用openresty redis和lua脚本 将每次调用企微服务器的请求以正确的公网ip出去

# 实现方式

1. 修改libWeWorkFinanceSdk_Java.so文件，将请求转发到openresty
2. openresty拦截/cgi-bin/gettoken接口将返回的access_token和corpid关系写入redis(也可让java单独实现，定时跑)
3. openresty请求企微时通过lua脚本查询redis获取access_token对应的corpid
4. 再通过一个内置的map关系文件拿到对应的上游地址去请求
5. pod使用阿里云功能绑定eip，实现pod单独公网ip出口

# 阿里云Pod配置出公网IP
新版k8s已经支持一组pod挂载同一个eip了
- 弹性公网IP支持为指定Pod配置出公网IP。具体操作，请参见[为Pod挂载独立公网EIP](https://help.aliyun.com/document_detail/200610.htm#task-2035433)。
- 公网NAT网关支持为一组Pod配置相同出公网IP。具体操作，请参见[配置Terway网络下节点级别网络](https://help.aliyun.com/document_detail/432964.htm?spm=a2c4g.470915.0.0.69e04e0fU3Np0m#task-2221756)和[为Pod配置固定IP及独立虚拟交换机、安全组](https://help.aliyun.com/document_detail/443632.htm?spm=a2c4g.470915.0.0.69e04e0fU3Np0m#task-2236831)。

# 流程图
![image](https://github.com/user-attachments/assets/be9a7fc4-6315-4713-a457-55d6b6b71a04)

# 遇到的坑

1. 不能使用openresty将resp body 请求写入redis，op只支持非阻塞性写入，但是可以通过异步来做(eof,但是有时效性问题，主要是会话存档)
2. 不能纯使用go语言代替,目前观察有个拉取图片和文件的接口/cgi-bin/message/getchatmediadata，企微sdk使用分片来做的，我用了gin还有其他的一些客户端试了不行，所以这个方案只有openresty来做
