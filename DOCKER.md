# Docker 部署指南

本文档介绍如何使用 Docker 运行 Drip 服务器。

## 快速开始

### 1. 构建镜像

```bash
# 基本构建
docker build -t drip-server .

# 带版本信息构建
docker build \
  --build-arg VERSION=v1.0.0 \
  --build-arg GIT_COMMIT=$(git rev-parse --short HEAD) \
  --build-arg BUILD_TIME=$(date -u '+%Y-%m-%d_%H:%M:%S') \
  -t drip-server:v1.0.0 \
  .

# 多架构构建 (需要 buildx)
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t drip-server:latest \
  --push \
  .
```

### 2. 运行服务器

#### 基本运行

```bash
docker run -d \
  --name drip-server \
  -p 80:80 \
  -p 8443:8443 \
  -e DRIP_DOMAIN=tunnel.example.com \
  -e DRIP_TOKEN=your-secret-token \
  drip-server
```

#### 使用自定义配置

```bash
docker run -d \
  --name drip-server \
  -p 80:80 \
  -p 8443:8443 \
  -p 20000-20100:20000-20100 \
  -e DRIP_DOMAIN=tunnel.example.com \
  -e DRIP_TOKEN=your-secret-token \
  -e DRIP_TCP_PORT_MIN=20000 \
  -e DRIP_TCP_PORT_MAX=20100 \
  -e DRIP_TRANSPORTS=tcp,wss \
  -e DRIP_TUNNEL_TYPES=http,https,tcp \
  drip-server
```

#### 使用 TLS 证书

```bash
docker run -d \
  --name drip-server \
  -p 80:80 \
  -p 443:443 \
  -p 8443:8443 \
  -v /path/to/certs:/var/lib/drip/certs:ro \
  -e DRIP_DOMAIN=tunnel.example.com \
  -e DRIP_TOKEN=your-secret-token \
  drip-server server \
    --port 8443 \
    --domain tunnel.example.com \
    --token your-secret-token \
    --tls-cert /var/lib/drip/certs/fullchain.pem \
    --tls-key /var/lib/drip/certs/privkey.pem
```

### 3. 查看日志

```bash
# 实时查看日志
docker logs -f drip-server

# 查看最近 100 行
docker logs --tail 100 drip-server
```

### 4. 停止和清理

```bash
# 停止服务器
docker stop drip-server

# 删除容器
docker rm drip-server

# 删除镜像
docker rmi drip-server
```

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `DRIP_PORT` | `8443` | 主服务器端口 |
| `DRIP_DOMAIN` | `tunnel.localhost` | 服务器域名 |
| `DRIP_TOKEN` | (空) | 认证令牌（强烈建议设置） |
| `DRIP_TCP_PORT_MIN` | `20000` | TCP 隧道最小端口 |
| `DRIP_TCP_PORT_MAX` | `40000` | TCP 隧道最大端口 |
| `DRIP_TRANSPORTS` | `tcp,wss` | 允许的传输协议 |
| `DRIP_TUNNEL_TYPES` | `http,https,tcp` | 允许的隧道类型 |

## 端口映射

| 容器端口 | 用途 |
|---------|------|
| `80` | HTTP 反向代理 |
| `443` | HTTPS 反向代理（可选） |
| `8443` | 主隧道服务器 (TLS) |
| `20000-40000` | 动态 TCP 隧道端口范围 |

**注意**: TCP 隧道需要映射整个端口范围。为了减少端口映射，可以缩小范围：

```bash
# 只映射 100 个端口
docker run -d \
  -p 80:80 \
  -p 8443:8443 \
  -p 20000-20099:20000-20099 \
  -e DRIP_TCP_PORT_MIN=20000 \
  -e DRIP_TCP_PORT_MAX=20099 \
  drip-server
```

## 数据持久化

### 使用配置文件

创建配置文件 `config.yaml`:

```yaml
server:
  port: 8443
  domain: tunnel.example.com
  token: your-secret-token

tcp:
  port_min: 20000
  port_max: 40000

transports:
  - tcp
  - wss

tunnel_types:
  - http
  - https
  - tcp
```

运行容器:

```bash
docker run -d \
  --name drip-server \
  -p 80:80 \
  -p 8443:8443 \
  -v $(pwd)/config.yaml:/etc/drip/config.yaml:ro \
  drip-server server -c /etc/drip/config.yaml
```

### 持久化证书

```bash
# 创建数据卷
docker volume create drip-certs

# 使用数据卷
docker run -d \
  --name drip-server \
  -v drip-certs:/var/lib/drip/certs \
  drip-server
```

## 生产部署建议

### 1. 使用反向代理 (Nginx/Caddy)

```nginx
# nginx.conf
server {
    listen 80;
    server_name *.tunnel.example.com tunnel.example.com;

    location / {
        proxy_pass http://drip-server:80;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}

server {
    listen 443 ssl http2;
    server_name *.tunnel.example.com tunnel.example.com;

    ssl_certificate /etc/letsencrypt/live/tunnel.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/tunnel.example.com/privkey.pem;

    location / {
        proxy_pass http://drip-server:80;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
}
```

### 2. 资源限制

```bash
docker run -d \
  --name drip-server \
  --memory=512m \
  --cpus=1.0 \
  -p 80:80 \
  -p 8443:8443 \
  drip-server
```

### 3. 自动重启

```bash
docker run -d \
  --name drip-server \
  --restart unless-stopped \
  -p 80:80 \
  -p 8443:8443 \
  drip-server
```

### 4. 健康检查

容器已内置健康检查，可以查看状态：

```bash
docker inspect --format='{{.State.Health.Status}}' drip-server
```

## 使用 Docker 网络

### 创建网络

```bash
docker network create drip-network
```

### 运行服务器

```bash
docker run -d \
  --name drip-server \
  --network drip-network \
  -p 80:80 \
  -p 8443:8443 \
  drip-server
```

### 运行 Nginx 反向代理

```bash
docker run -d \
  --name nginx \
  --network drip-network \
  -p 80:80 \
  -p 443:443 \
  -v $(pwd)/nginx.conf:/etc/nginx/nginx.conf:ro \
  -v /etc/letsencrypt:/etc/letsencrypt:ro \
  nginx:alpine
```

## 多架构支持

镜像支持以下架构：
- `linux/amd64` (x86_64)
- `linux/arm64` (ARM64/aarch64)

Docker 会自动选择匹配的架构。

## 故障排查

### 查看日志

```bash
# 服务器日志
docker logs drip-server

# 实时日志
docker logs -f drip-server

# JSON 格式的结构化日志
docker logs drip-server | grep '"level":"error"'
```

### 进入容器

```bash
docker exec -it drip-server sh
```

### 测试连接

```bash
# 测试服务器是否运行
curl http://localhost/health

# 测试 TLS 连接
openssl s_client -connect localhost:8443 -servername tunnel.example.com
```

### 常见问题

**问题**: 容器无法启动
```bash
# 检查日志
docker logs drip-server

# 检查端口是否被占用
netstat -tlnp | grep 8443
```

**问题**: 客户端无法连接
```bash
# 确保防火墙允许端口
sudo ufw allow 8443/tcp

# 检查容器网络
docker inspect drip-server | grep IPAddress
```

**问题**: TCP 隧道无法工作
```bash
# 确保映射了完整的端口范围
docker port drip-server

# 重新运行时映射所需端口
docker rm -f drip-server
docker run -d -p 20000-20100:20000-20100 ...
```

## 安全建议

1. **始终设置强密码令牌**: `-e DRIP_TOKEN=strong-random-token`
2. **使用 TLS 证书**: 挂载 Let's Encrypt 证书
3. **限制传输协议**: 如果不需要 WebSocket，设置 `-e DRIP_TRANSPORTS=tcp`
4. **限制隧道类型**: 如果只需要 HTTP，设置 `-e DRIP_TUNNEL_TYPES=http`
5. **配置防火墙**: 只开放必要的端口
6. **定期更新**: `docker pull` 获取最新镜像

## 性能调优

### 增加文件描述符限制

```bash
docker run -d \
  --ulimit nofile=65535:65535 \
  drip-server
```

### 使用 Host 网络模式 (生产环境)

```bash
docker run -d \
  --network host \
  -e DRIP_PORT=8443 \
  drip-server
```

**注意**: Host 模式下容器直接使用宿主机网络，性能最佳但安全性略低。

## 示例：完整的生产部署

```bash
#!/bin/bash

# 1. 停止旧容器
docker stop drip-server 2>/dev/null
docker rm drip-server 2>/dev/null

# 2. 拉取/构建最新镜像
docker build -t drip-server:latest .

# 3. 启动新容器
docker run -d \
  --name drip-server \
  --restart unless-stopped \
  --memory=1g \
  --cpus=2.0 \
  --ulimit nofile=65535:65535 \
  -p 80:80 \
  -p 443:443 \
  -p 8443:8443 \
  -p 20000-20100:20000-20100 \
  -v /etc/letsencrypt:/var/lib/drip/certs:ro \
  -e DRIP_DOMAIN=tunnel.example.com \
  -e DRIP_TOKEN=your-super-secret-token-here \
  -e DRIP_TCP_PORT_MIN=20000 \
  -e DRIP_TCP_PORT_MAX=20100 \
  -e DRIP_TRANSPORTS=tcp,wss \
  -e DRIP_TUNNEL_TYPES=http,https,tcp \
  drip-server:latest server \
    --port 8443 \
    --domain tunnel.example.com \
    --token your-super-secret-token-here \
    --tls-cert /var/lib/drip/certs/live/tunnel.example.com/fullchain.pem \
    --tls-key /var/lib/drip/certs/live/tunnel.example.com/privkey.pem

# 4. 等待启动
sleep 5

# 5. 检查状态
docker ps | grep drip-server
docker logs --tail 20 drip-server

echo "Drip server started successfully!"
echo "Test: curl http://localhost/health"
```

## 客户端连接

启动服务器后，客户端可以这样连接：

```bash
# 配置客户端
drip config init

# 启动 HTTP 隧道
drip http 3000

# 使用自定义域名 (CNAME)
drip http 3000 --custom-host www.example.com

# 强制使用 WebSocket (CDN 模式)
drip http 3000 --transport wss
```

---

**更多信息请参考**: [CLAUDE.md](./CLAUDE.md) 和 [README.md](./README.md)
