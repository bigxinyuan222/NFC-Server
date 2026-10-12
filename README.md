# checkin-server

基于 Go 标准库 `net/http` 的打卡后端服务。用户登录后在指定签到点刷卡打卡，本地记录落 MySQL，后台 worker 异步把每一条打卡流水同步到钉钉考勤。

## 功能

- 用户注册 / 登录，口令使用 PBKDF2 哈希存储，登录后下发 JWT（HS256，72 小时有效期）。
- 签到：校验 JWT、刷卡卡号白名单、签到点存在且启用，写入 `checkin_log`。
- 我的签到记录：按 JWT 中的用户 ID 分页查询，不接受前端传入的 `userId`。
- 钉钉考勤同步：本地签到成功后写入上传队列，后台 worker 每 5 秒轮询，失败按指数退避重试，成功后回写钉钉 `request_id`。
- 钉钉用户映射：注册时可直接填写 `dingTalkUserId`；未填写时按 `mobile` 调用 `getbymobile` 反查；启动时还会分页拉取通讯录批量补全映射。
- 内嵌 OpenAPI 规范：启动后访问 `GET /openapi.json`。

## 技术栈

- Go 1.21，仅依赖 `database/sql` + 标准库 `net/http`，无 Web 框架。
- MySQL（驱动 `go-sql-driver/mysql`）。
- JWT 库 `golang-jwt/jwt/v5`。

## 目录结构

```
checkin-server/
├── main.go                # HTTP 入口、路由、登录/注册/签到/我的记录
├── password.go            # PBKDF2 哈希与校验
├── dingtalk.go            # 钉钉客户端、通讯录同步、上传队列 worker
├── openapi.json           # 接口规范（通过 go:embed 内嵌）
├── init.sql               # 全新环境建库建表脚本
├── migrations/            # 增量迁移，按编号顺序执行
├── dingtalk.local.env      # 本地钉钉凭证（已 gitignore，不入库）
├── start-server.ps1       # Windows 启动脚本
└── *_test.go              # 单元测试
```

## 快速开始

### 1. 前置依赖

- Go 1.21+
- MySQL 5.7+ / 8.0（需要 utf8mb4、外键约束、CHECK 约束，MySQL 8.0.16+）

### 2. 初始化数据库

全新环境直接执行：

```bash
mysql -u root -p < init.sql
```

已有老库按编号顺序补跑 `migrations/` 下的 SQL（详见 `DINGTALK.md` 中列出的必需迁移）。

### 3. 配置环境变量

必需：

```powershell
$env:DB_DSN     = "user:password@tcp(127.0.0.1:3306)/checkin_db?parseTime=true&charset=utf8mb4"
$env:JWT_SECRET = "<至少 32 个字符的随机串>"
```

可选：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LISTEN_ADDR` | `0.0.0.0:8080` | 监听地址 |
| `DINGTALK_CLIENT_ID` | 空 | 钉钉应用 Client ID |
| `DINGTALK_CLIENT_SECRET` | 空 | 钉钉应用 Client Secret |
| `DINGTALK_ACCESS_TOKEN` | 空 | 临时静态 Token，配置后优先于 Client ID/Secret |
| `DINGTALK_DEVICE_NAME` / `DINGTALK_DEVICE_ID` | — | 上传考勤时的设备标识 |
| `DINGTALK_UPLOAD_FORMAT` | `form` | 上传接口编码，企业若要求 JSON 改为 `json` |
| `DINGTALK_SYNC_USERS_ON_START` | `true` | 启动时是否批量同步通讯录映射 |
| `DINGTALK_ROOT_DEPT_ID` | `1` | 通讯录同步起始部门 |
| `DINGTALK_PROXY_URL` | 空 | 钉钉请求代理；默认直连，不继承系统代理变量 |

钉钉凭证也可写入工作目录下的 `dingtalk.local.env`（键值每行一项），`start-server.ps1` 会自动加载；该文件已加入 `.gitignore`。完整钉钉配置与权限说明见 [DINGTALK.md](./DINGTALK.md)。

未配置任何钉钉凭证时服务仍可本地打卡，响应里 `dingtalkSync` 会返回 `disabled`。

### 4. 编译与启动

```powershell
go build -o checkin-server.exe .
.\start-server.ps1
```

启动脚本会校验 `DB_DSN` / `JWT_SECRET` 与钉钉凭证是否齐全，清理可能残留的系统代理变量，然后后台拉起 `checkin-server.exe`。加 `-Wait` 可在前台运行便于看日志。

## 命令行子命令

```powershell
# 生成密码哈希（从 stdin 读明文，适合直接写库时使用）
echo "your-password" | .\checkin-server.exe hash-password

# 一次性把钉钉企业通讯录分页导入本地 user 表（幂等，不覆盖已有密码）
.\checkin-server.exe sync-dingtalk-users
```

## HTTP 接口

所有接口均返回 `{ "code": 0, "msg": "...", "data": ... }` 结构，`code=0` 表示成功。

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| POST | `/api/register` | 无 | 注册用户，body：`userName`、`password`、`department`、可选 `mobile`、`dingTalkUserId` |
| POST | `/api/login` | 无 | 用户名密码登录，返回 JWT 与用户基本信息 |
| POST | `/api/checkin` | Bearer JWT | 签到，body：`pointId`、`uid`（刷卡卡号，自动转大写并对白名单） |
| GET  | `/api/checkins/me` | Bearer JWT | 分页查询本人打卡记录，query：`page`、`pageSize`（≤100） |
| GET  | `/openapi.json` | 无 | 返回内嵌的 OpenAPI 规范 |

需要鉴权的接口在请求头携带：

```
Authorization: Bearer <token>
```

## 测试

```powershell
go test ./...
```

覆盖登录/注册/签到逻辑、密码哈希与钉钉客户端（`dingtalk_test.go` 使用 mock 上传器，不依赖真实钉钉环境）。

## 备注

- 刷卡卡号白名单当前硬编码在 `main.go` 的 `checkinCardWhitelist`，新增卡片需要改代码后重新编译。
- 钉钉上传只接受最近 180 天内的记录，`user_check_time` 取服务端收到签到时的本地时间。
