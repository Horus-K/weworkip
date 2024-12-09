# weworkip

本仓库提供一种企微自建应用saas(包括会话存档)解决方案

利用openresty redis和lua脚本 将每次调用企微服务器的请求以正确的公网ip出去

# 实现方式

1. 修改libWeWorkFinanceSdk_Java.so文件，将请求转发到openresty
2. openresty拦截/cgi-bin/gettoken接口将返回的access_token和corpid关系写入redis(也可让java单独实现，定时跑)
3. openresty请求企微时通过lua脚本查询redis获取access_token对应的corpid
4. 再通过一个内置的map关系文件拿到对应的上游地址去请求

# 流程图
![image](https://github.com/user-attachments/assets/be9a7fc4-6315-4713-a457-55d6b6b71a04)

# 遇到的坑

1. 不能使用openresty将resp body 请求写入redis，op只支持非阻塞性写入，但是可以通过异步来做(eof,但是有时效性问题，主要是会话存档)
2. 不能纯使用go语言代替,目前观察有个拉取图片和文件的接口/cgi-bin/message/getchatmediadata，企微sdk使用分片来做的，我用了gin还有其他的一些客户端试了不行，所以这个方案只有openresty来做
