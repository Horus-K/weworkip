package main

import (
    "bufio"
    "bytes"
    "compress/gzip"
    "context"
    "encoding/json"
    "flag"
    "fmt"
    "github.com/gin-gonic/gin"
    "github.com/go-redis/redis/v8"
    "github.com/google/uuid"
    "github.com/valyala/fasthttp"
    "go.uber.org/zap"
    "go.uber.org/zap/zapcore"
    "gopkg.in/natefinch/lumberjack.v2"
    "io"
    "net/http"
    "os"
    "strings"
    "time"
)

var sugar *zap.SugaredLogger

// LoggerZap 中间件，用于将日志记录到 zap
func LoggerZap(sugar *zap.SugaredLogger) gin.HandlerFunc {
    return func(c *gin.Context) {
        start := time.Now()
        // 处理请求
        c.Next()
        corpid, _ := c.Get("Corpid")
        upstreamResponseTime, _ := c.Get("upstream_response_time")
        upstreamAddr, _ := c.Get("upstream_addr")
        requestID, _ := c.Get("requestID")
        // 记录详细请求日志
        sugar.Infow("Request log",
            "remote", c.Request.RemoteAddr,
            "status", c.Writer.Status(),
            "xff", c.Request.Header.Get("X-Forwarded-For"),
            "corpid", corpid,
            "user_agent", c.Request.UserAgent(),
            "upstream", c.Request.Host,
            "method", c.Request.Method,
            "url", c.Request.URL.String(),
            "ip", c.ClientIP(),
            "time", time.Since(start),
            "upstream", upstreamAddr,
            "upstream_response_time", upstreamResponseTime,
            "content_length", c.Writer.Header().Get("Content-Length"),
            "requestID", requestID,
        )
    }
}

func writeRedis(rdb *redis.Client, c *gin.Context, body []byte) {
    requestID, _ := c.Get("requestID")
    sugar := sugar.With("requestID", requestID)
    var ctx = context.Background()
    if c.Writer.Header().Get("Content-Encoding") == "gzip" {
        // 解压缩 Gzip 数据
        sugar.Debugf("开始解压Gzip")
        bodyReader := bytes.NewReader(body)
        reader, err := gzip.NewReader(bodyReader)
        if err != nil {
            sugar.Warn("无法创建 Gzip Reader:", err)
            c.JSON(http.StatusInternalServerError, gin.H{"error": "无法解压请求体"})
            return
        }
        defer reader.Close()
        // 读取解压后的数据
        decompressedBody, err := io.ReadAll(reader)
        if err != nil {
            sugar.Warn("无法读取解压数据:", err)
            c.JSON(http.StatusInternalServerError, gin.H{"error": "无法读取解压数据"})
            return
        }
        // 打印解压后的请求体
        bodyString := string(decompressedBody)
        sugar.Debugf("解压后的请求体内容: %s", bodyString)
    }
    corpid := c.Request.URL.Query().Get("corpid")
    // 打印返回的 JSON 内容
    var jsonResponse map[string]interface{}
    if err := json.Unmarshal(body, &jsonResponse); err != nil {
        sugar.Warnf("无法解析返回数据: %s,原始BODY为: %s", err, body)
        return
    } else {
        // 检查 access_token 是否存在
        if accessToken, exists := jsonResponse["access_token"]; exists {
            sugar.Debugf("corpid:%s access_token存在: %s", corpid, accessToken)
            //将响应体写入 Redis
            redisKey := fmt.Sprintf("qwtoken:%s", accessToken)
            redisValue := corpid
            err = rdb.Set(ctx, redisKey, redisValue, 24*time.Hour).Err()
            if err != nil {
                sugar.Warn("Redis写入失败", err)
                err = rdb.Set(ctx, redisKey, redisValue, 24*time.Hour).Err()
                if err != nil {
                    c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not write to Redis"})
                    sugar.Warn("Redis第二次写入失败", err)
                    return
                }
            } else {
                sugar.Debugf("写入redis 企业: %s 密钥 %s", corpid, accessToken)
            }
        } else {
            sugar.Warnf("无法解析access_token: %s,原始BODY为: %s", err, body)
            return
        }
    }
}

func retryFastHttp(rdb *redis.Client, c *gin.Context, fastHttpClient *fasthttp.Client, url string) ([]byte, error) {
    requestID, _ := c.Get("requestID")
    sugar := sugar.With("requestID", requestID)
    var err error
    req := fasthttp.AcquireRequest()
    defer fasthttp.ReleaseRequest(req)
    resp := fasthttp.AcquireResponse()
    defer fasthttp.ReleaseResponse(resp)
    req.Header.SetMethod(c.Request.Method)
    bodyBytes, err := io.ReadAll(c.Request.Body) // 读取请求体
    defer c.Request.Body.Close()                 // 确保在函数退出时关闭原始请求体
    req.SetBody(bodyBytes)
    // 设置请求的目标 URL
    req.SetRequestURI(url + c.Request.RequestURI)
    // 复制客户端请求头到 fasthttp 请求
    for key, values := range c.Request.Header {
        for _, value := range values {
            req.Header.Add(key, value)
        }
    }
    // 记录开始时间
    startTime := time.Now()
    for i := 0; i < 3; i++ {
        sugar.Debugf("第%d次请求", i+1)
        // 发起请求
        err = fastHttpClient.Do(req, resp)
        // 计算上游响应时间
        upstreamTime := time.Since(startTime)
        c.Set("upstream_response_time", upstreamTime)
        if err != nil || resp.StatusCode() >= 500 {
            sugar.Warn("Request failed: %v", err)
            time.Sleep(time.Duration(15) * time.Second)
            continue
        }
        break
    }
    if err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": "请求错误"})
        return nil, err
    }
    writeRedis(rdb, c, resp.Body())
    // 复制响应头到 Gin 上下文
    resp.Header.VisitAll(func(key, value []byte) {
        c.Writer.Header().Set(string(key), string(value))
    })
    c.Status(resp.StatusCode())
    bodyReader := bytes.NewReader(resp.Body())
    if _, err := io.Copy(c.Writer, bodyReader); err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": "响应传输失败"})
    }
    return resp.Body(), nil
}
func parseLine(line string, result map[string]string) {
    // 去除末尾的分号并分割字符串
    entries := strings.Split(strings.TrimSuffix(line, ";"), ";")

    for _, entry := range entries {
        // 去除空白字符并分割键值
        entry = strings.TrimSpace(entry)
        if entry == "" {
            continue
        }

        parts := strings.Fields(entry)
        if len(parts) == 2 {
            // 去除引号并添加到 map 中
            key := strings.Trim(parts[0], `"`)
            value := strings.Trim(parts[1], `"`)
            result[key] = value
        }
    }
}
func main() {
    redisAddr := flag.String("redis_addr", "127.0.0.1:6379", "redisAddr")
    redisUser := flag.String("redis_user", "", "redisUser")
    redisPassword := flag.String("redis_pw", "", "redisPassword")
    redisDB := flag.Int("redis_db", 3, "redisDB")
    qywxHost := flag.String("qywx_host", "https://qyapi.weixin.qq.com", "默认企微请求地址")
    bu := flag.String("before_upStream", "http://", "upStream前缀")
    ab := flag.String("after_upStream", ".suosi-pre.svc.cluster.local:20000", "upStream后缀")
    debugMode := flag.String("log_mode", "release", "日志模式: debug, release")
    ginMode := flag.String("gin_mode", "release", "gin模式: debug, release")
    serverPort := flag.String("port", "8080", "启动端口")
    openLogFile := flag.Bool("log_file", false, "是否写日志文件")
    logMaxSize := flag.Int("log_maxsize", 300, "文件大小MB")
    logMaxAge := flag.Int("log_maxage", 10, "保留旧文件的最大天数")
    logMaxBackups := flag.Int("log_maxbackups", 10, "保留旧文件的最大数量")
    logCompress := flag.Bool("log_compress", true, "是否压缩日志")
    flag.Parse()
    if *ginMode == "release" {
        gin.SetMode(gin.ReleaseMode)
    } else {
        gin.SetMode(gin.DebugMode)
    }
    var err error
    // 设置日志级别
    var logConf zap.Config
    var logger *zap.Logger
    loc, _ := time.LoadLocation("Asia/Shanghai")
    if *debugMode == "debug" {
        //logConf = zap.NewDevelopmentConfig() // 开发模式，输出 debug 日志
        logConf.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
        logConf.Development = false
        logConf.Sampling = &zap.SamplingConfig{
            Initial:    100,
            Thereafter: 100,
        }
        logConf.Encoding = "json"
        logConf.EncoderConfig = zap.NewProductionEncoderConfig()
    } else {
        logConf = zap.NewProductionConfig() // 生产模式，输出 info 及以上级别日志
    }
    logConf.EncoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
        enc.AppendString(t.In(loc).Format("2006-01-02 15:04:05.000"))
    }
    if *openLogFile {
        // 将输出设置为文件
        logConf.OutputPaths = []string{"stdout"}
        logConf.ErrorOutputPaths = []string{"stderr"}
        // 配置 lumberjack
        lumberjackLogger := &lumberjack.Logger{
            Filename:   "app.log",
            MaxSize:    *logMaxSize,    // MB
            MaxBackups: *logMaxBackups, // 保留旧文件的最大数量
            MaxAge:     *logMaxAge,     // 保留旧文件的最大天数
            Compress:   *logCompress,   // 是否压缩/归档旧文件
        }
        // 创建 zapcore.Core
        core := zapcore.NewCore(
            zapcore.NewJSONEncoder(logConf.EncoderConfig),
            zapcore.AddSync(lumberjackLogger),
            logConf.Level,
        )
        // 添加标准输出
        consoleCore := zapcore.NewCore(
            zapcore.NewJSONEncoder(logConf.EncoderConfig), // 可选择使用 PlainEncoder
            zapcore.AddSync(os.Stdout),                    // 标准输出
            logConf.Level,
        )
        // 创建 logger
        logger = zap.New(zapcore.NewTee(core, consoleCore))
    } else {
        logConf.OutputPaths = []string{"stdout"}
        logConf.ErrorOutputPaths = []string{"stderr"}
        logger, err = logConf.Build()
    }
    if err != nil {
        panic(err)
    }
    defer logger.Sync() // 确保日志写入
    sugar = logger.Sugar()
    sugar.Infof("当前版本: v1.2.0")

    corpidMapsName := "gin_conf/cropid_map.map"
    sugar.Infof("使用本地文件中配置")

    // 打开文件
    file, err := os.Open(corpidMapsName) // 替换为您的文件名
    if err != nil {
        fmt.Println("无法打开文件:", err)
        return
    }
    defer file.Close()
    // 解析文件为 map
    corpids := make(map[string]string)
    scanner := bufio.NewScanner(file)
    for scanner.Scan() {
        line := scanner.Text()
        if line == "" {
            continue // 跳过空行
        }
        parseLine(line, corpids)
    }
    // 检查扫描错误
    if err := scanner.Err(); err != nil {
        fmt.Println("读取文件时出错:", err)
    }
    // 打印结果
    fmt.Println(corpids)
    //if err != nil {
    //	panic("初始化配置文件加载失败" + err.Error())
    //}
    sugar.Infof("路由配置文件: %+v", corpids)
    //if config.BaseURL == "" {
    //	panic(ginConfName + "配置加载不正确")
    //}
    rdb := redis.NewClient(&redis.Options{
        Addr:     *redisAddr,
        Username: *redisUser,
        Password: *redisPassword,
        DB:       *redisDB,
    })
    // 检查 Redis 连接是否成功
    var ctx = context.Background()
    _, err = rdb.Ping(ctx).Result()
    if err != nil {
        sugar.Warn("连接redis异常:", err)
        return
    }
    sugar.Info("连接redis成功!")
    defer rdb.Close() // 在程序结束时关闭 Redis 连接
    r := gin.New()
    r.Use(LoggerZap(sugar))
    // 全局的 fasthttp.Client
    var fastHttpClient = &fasthttp.Client{}
    // 中间件：为每个请求生成 UUID
    r.Use(func(c *gin.Context) {
        reqID := uuid.New().String() // 生成唯一请求 ID
        c.Set("requestID", reqID)    // 将请求 ID 存入上下文
        // 在处理请求后继续
        c.Next()
    })
    r.Any("/*path", func(c *gin.Context) {
        requestID, _ := c.Get("requestID")
        sugar := sugar.With("requestID", requestID)
        sugar.Debugf("客户端请求内容: %+v", c.Request)
        // 判断路径
        path := c.Param("path")
        switch path {
        case "/check_gin_proxy":
            // 检查redis
            var ctx = context.Background()
            _, err = rdb.Ping(ctx).Result()
            if err != nil {
                sugar.Fatalf("连接redis异常: %s", err)
            }
            c.JSON(http.StatusOK, gin.H{"status": "ok"})
        case "/cgi-bin/gettoken":
            // 设置请求头, 不允许压缩
            c.Request.Header.Add("Accept-Encoding", "identity")
            // 获取corpid
            corpid := c.Request.URL.Query().Get("corpid")
            // 获取配置中的代理地址
            upStream := ""
            if svc, exists := corpids[corpid]; exists {
                upStream = svc
            }
            reqAddr := ""
            if upStream != "" {
                reqAddr = *bu + upStream + *ab
            } else {
                reqAddr = *qywxHost
            }
            c.Set("Corpid", corpid)
            c.Set("upstream_addr", reqAddr)
            sugar.Debugf("地址为: %s, corpid: %s", upStream, corpid)
            _, err := retryFastHttp(rdb, c, fastHttpClient, reqAddr)
            if err != nil {
                sugar.Warn("直接请求失败" + err.Error())
                c.JSON(http.StatusInternalServerError, gin.H{"error": "直接请求失败" + err.Error()})
                return
            }
        case "/check":
            c.JSON(http.StatusNotFound, gin.H{"error": 0})
        default:
            c.JSON(http.StatusOK, gin.H{"error": "不支持此请求"})
        }
    })
    server := "0.0.0.0:" + *serverPort
    err = r.Run(server)
    if err != nil {
        return
    }
}
