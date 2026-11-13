# bot-mcp

推推 MCP 服务。把推推 Bot 能力封装为 MCP，基于推推 Bot SDK 开发。

## 编译与发布

```sh
go build -trimpath .
./bot-mcp
```

默认监听 `65460` 端口，可选参数 `-listen` 可修改监听地址。

日志写入 `log/YYYY-MM-DD.log`，自动创建目录，保留最近 7 天。

也可使用容器：

```sh
docker build -t bot-mcp .
docker run --rm -p 65460:65460 bot-mcp
```

## 调用

MCP 端点为 `/mcp`。通过 URL 参数鉴权：

```text
http://SERVER_HOST:65460/mcp?appid=APP_ID&secret=APP_SECRET
```

也支持 HTTP Header鉴权：

```text
Authorization: Bearer APP_ID-APP_SECRET
```

URL 参数优先于 header。

## 测试

```sh
go test ./...
go vet ./...
```

真实 E2E测试：复制 `.env.e2e.sample` 为 `.env.e2e`，填写测试密钥及测试配置：

```sh
go run . -e2e
```
