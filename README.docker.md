# 使用 Docker 运行 OpeniLink Hub

## 构建并启动

```bash
docker compose up -d --build
```

构建失败时用 `--progress=plain` 查看具体失败步骤（Docker 默认会折叠输出）：

```bash
docker compose build --progress=plain
# 或
docker build --progress=plain -t openilink-hub .
```

多阶段构建：

1. `web-builder`（node:22-bookworm-slim / glibc）：构建前端，产物输出到 `internal/web/dist`（随后内嵌进 Go 二进制）。**不要改成 alpine**：vite-plus（rolldown）的原生 binding 在 musl 下装不上，会报 `Cannot find module 'vite-plus.linux-x64-musl.node'`；
2. `go-builder`（golang:1.25-bookworm）：以 CGO 编译后端（provider/ilink 含 silk C 代码，需要 gcc），并嵌入前端产物；
3. 运行时镜像（debian:bookworm-slim，glibc）。

启动后访问 http://localhost:9800。

## 部署到 Zeabur

1. 在 Zeabur 创建项目 → 添加服务 → **Git Service**，导入本仓库（`Tom6814/Nope`）。Zeabur 会自动检测根目录的 `Dockerfile` 并用它构建（构建机 2 vCPU / 4 GB，构建失败时在部署日志里能看到具体错误行）。
2. **端口**：程序会读取 Zeabur 注入的 `PORT` 环境变量（也兼容 `LISTEN`），无需额外配置。
3. **数据库（必须用 PostgreSQL）**：Zeabur 容器文件系统是临时的，SQLite 文件会在重启后丢失。请在项目里添加一个 PostgreSQL 服务，然后把连接串设为 `DATABASE_URL`：
   ```
   DATABASE_URL=postgres://user:pass@host:5432/openilink?sslmode=disable
   ```
4. **密钥**：设置 `SECRET`（GitHub webhook 签名校验）。
5. 可选：`RP_ORIGIN` / `RP_ID` 按你的域名配置（WebAuthn）。

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LISTEN` | `:9800` | HTTP 监听地址（容器内默认已是 `:9800`） |
| `DATABASE_URL` | 空（使用 SQLite） | 数据库连接串；留空使用默认 SQLite，设为 PostgreSQL URL 则改用 Postgres |
| `SECRET` | `change-me-in-production` | GitHub webhook 签名校验密钥，生产环境务必覆盖：`SECRET=你的密钥 docker compose up -d` |
| `RP_ORIGIN` | `http://localhost:9800` | WebAuthn 依赖方 Origin |
| `RP_ID` | `localhost` | WebAuthn 依赖方 ID |
| `RP_NAME` | `OpeniLink Hub` | WebAuthn 依赖方显示名称 |

## 数据持久化（SQLite）

默认使用 SQLite，数据库文件位于容器内 `/var/lib/openilink-hub/openilink.db`，已通过命名卷 `openilink-data` 持久化：

```bash
docker compose down      # 停止容器，数据卷保留
docker compose down -v   # 停止并删除数据卷（数据会丢失！）
```

如需备份，建议**先停服**再拷贝 `openilink.db`（SQLite 启用了 WAL，运行中直接拷贝可能缺失 WAL 数据），或使用 `sqlite3 openilink.db ".backup backup.db"`。

## 可选：使用 PostgreSQL

1. 先准备一个 PostgreSQL 实例（本 compose 未内置，可自行启动 postgres 容器）；
2. 设置 `DATABASE_URL` 指向它并重新创建容器：

```bash
DATABASE_URL="postgres://openilink:pass@your-host:5432/openilink?sslmode=disable" \
  docker compose up -d --build
```

启动时自动执行数据库迁移。
