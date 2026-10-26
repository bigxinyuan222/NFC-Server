# 钉钉签到同步

本服务把每一条本地 `checkin_log` 作为一条第三方打卡流水上传到钉钉，钉钉会继续按企业考勤组和班次重新计算考勤结果。

## 配置

```powershell
$env:DINGTALK_CLIENT_ID = "钉钉应用 Client ID"
$env:DINGTALK_CLIENT_SECRET = "钉钉应用 Client Secret"
# 原有凭证对应乐知教育：state=org_lezhiedu
$env:DINGTALK_ORG_LEZHIDATA_CLIENT_ID = "乐知数字应用 Client ID"
$env:DINGTALK_ORG_LEZHIDATA_CLIENT_SECRET = "乐知数字应用 Client Secret"
# 默认已有用户归属乐知教育
$env:DINGTALK_DEFAULT_ORG = "org_lezhiedu"
$env:DINGTALK_DEVICE_NAME = "外部系统同步"
$env:DINGTALK_DEVICE_ID = "manual_sync_001"
# 用户提供的接口约定为 form；如果企业接口实际要求 JSON，改为 json
$env:DINGTALK_UPLOAD_FORMAT = "form"
# 默认启动时批量同步通讯录；如需关闭可设为 "false"
$env:DINGTALK_SYNC_USERS_ON_START = "true"
# 默认从根部门 1 开始分页读取通讯录
$env:DINGTALK_ROOT_DEPT_ID = "1"
# 钉钉请求默认直连，不继承系统 HTTP_PROXY/HTTPS_PROXY/ALL_PROXY
# 如部署环境确实需要代理，再显式配置：
# $env:DINGTALK_PROXY_URL = "http://proxy.example:8080"
```

服务会使用 Client ID 和 Client Secret 请求 `gettoken`，并在有效期内缓存 Access Token。也可以临时直接配置
`DINGTALK_ACCESS_TOKEN` 作为静态 Token（它会优先于 Client ID/Secret）。没有配置任何一组凭证时，服务仍可本地签到，但响应中的 `dingtalkSync` 会是 `disabled`。

服务同时保留两个组织：原有 `DINGTALK_CLIENT_ID/SECRET` 对应 `org_lezhiedu`（乐知教育），
`DINGTALK_ORG_LEZHIDATA_CLIENT_ID/SECRET` 对应 `org_lezhidata`（乐知数字）。钉钉第三方登录请求体必须携带
`state`，值只能是 `org_lezhiedu` 或 `org_lezhidata`；后端会用该组织的 Client ID/Secret 换取用户身份，
并按相同组织查询本地用户。未知组织不会回退到另一套凭证。

凭证只通过环境变量注入，不要提交到 Git。若凭证曾经出现在聊天记录、日志或代码中，应先在钉钉管理后台重置 Client Secret，再配置新值。

当前签到不调用钉钉考勤结果读取接口。每次前端请求一次签到接口，后端会为同一个本地签到创建两条独立的钉钉流水上传任务，分别进入队列并独立重试。

## 用户映射

钉钉上传接口必须使用钉钉企业内的 `userid`，不能使用本地用户 ID、姓名或手机号。用户注册时可以提供：

```json
{
  "userName": "zhangsan",
  "password": "password",
  "department": "研发部",
  "mobile": "13800138000",
  "dingTalkUserId": "可选，已知时直接填写",
  "dingTalkOrg": "org_lezhiedu"
}
```

钉钉登录请求示例：`{"authCode":"...","state":"org_lezhiedu"}`。新用户属于乐知数字时注册时传
`"dingTalkOrg":"org_lezhidata"`；不传则使用 `DINGTALK_DEFAULT_ORG`，当前默认为乐知教育。

如果没有填写 `dingTalkUserId`，但填写了 `mobile`，第一次同步时服务会调用钉钉：

```text
POST https://oapi.dingtalk.com/topapi/v2/user/getbymobile
```

获取 `result.userid` 并保存到 `user.dingtalk_userid`。如果手机号和 userid 都没有，签到仍会保存到本地，但不会上传，响应状态为 `skipped_missing_mapping`。
手机号反查成功后，后续签到直接使用已保存的 `user.dingtalk_userid`，不再重复调用手机号接口。

服务启动时还会调用钉钉通讯录分页接口：

```text
POST https://oapi.dingtalk.com/topapi/v2/user/list
```

服务从 `DINGTALK_ROOT_DEPT_ID`（默认 `1`）开始按 100 条一页读取成员，用成员手机号匹配本地 `user.mobile`，匹配成功后写入 `user.dingtalk_userid`。批量同步失败只记录日志，不会阻断本地服务启动；没有匹配到的用户仍会在首次签到时按手机号回退查询。

如果需要把钉钉企业通讯录全部导入本地 `user` 表，应用必须开通 `qyapi_get_department_member`（通讯录成员读取）和 `qyapi_get_department_list`（部门列表读取）权限。仅有 `qyapi_get_member_by_mobile` 只能按已知手机号查询单个 UserID，不能获取全体成员。

通讯录列表没有返回手机号时，导入命令会继续调用 `topapi/v2/user/get` 补取详情，这一步还需要 `qyapi_get_member`（成员详情读取）权限。

开通权限后，在服务停止或独立维护窗口执行：

```powershell
.\checkin-server.exe sync-dingtalk-users
```

该命令会分页读取通讯录并幂等写入：已有用户补全 `mobile` 和 `dingtalk_userid`，不存在的用户新增到 `user` 表。新导入用户的本地密码是不可登录占位值，需要管理员后续设置密码；已有用户密码不会被覆盖。

已有数据库需要执行迁移：

```sql
SOURCE migrations/005_add_dingtalk_user_mapping.sql;
SOURCE migrations/006_create_dingtalk_upload_queue.sql;
SOURCE migrations/007_add_user_mobile.sql;
SOURCE migrations/008_drop_checkin_log_location.sql;
SOURCE migrations/009_allow_two_dingtalk_uploads_per_checkin.sql;
SOURCE migrations/011_add_checkin_log_device_id.sql;
SOURCE migrations/012_add_dingtalk_organization.sql;
```

## 同步流程

1. 本地签到成功后写入 `checkin_log`。
2. 若已配置钉钉且用户已映射，为同一次签到写入两条 `dingtalk_upload` 队列记录。
3. 后台 worker 每 5 秒分别处理两条队列记录，向 `attendance/record/upload` 发送 `userid`、设备信息和毫秒时间戳。
4. 失败记录按指数退避独立重试，成功后保存对应钉钉 `request_id`。

`user_check_time` 使用本地签到发生时的服务器时间，接口只接受最近 180 天内的记录。

## 标准启动与打卡流程

服务启动前，必须在当前进程环境中提供以下变量（不要把真实值写入代码、脚本或 Git）：

```powershell
$env:DB_DSN = "<MySQL DSN>"
$env:JWT_SECRET = "<至少 32 个字符>"
$env:DINGTALK_CLIENT_ID = "<Client ID>"
$env:DINGTALK_CLIENT_SECRET = "<Client Secret>"
$env:LISTEN_ADDR = "0.0.0.0:8080"
.\start-server.ps1
```

也可以将钉钉凭证保存在工作目录的 `dingtalk.local.env` 中。启动脚本会自动读取该文件；文件已加入 `.gitignore`，不会提交到版本库。数据库 DSN 和 JWT 密钥仍要求通过环境变量提供。

`start-server.ps1` 会检查必需配置、清除失效的系统代理变量并启动已编译的服务。默认监听 `0.0.0.0:8080`；如需指定网卡或端口，可设置 `LISTEN_ADDR`。

一次完整签到的状态流转如下：

1. 客户端 `POST /api/login` 获取 JWT。
2. 客户端携带 `Authorization: Bearer <JWT>` 调用 `POST /api/checkin`，提交 `pointId`。
3. 服务端检查 `pointId` 对应的签到点是否存在且启用。
4. 签到点有效后写入 `checkin_log`，然后根据本地用户的 `dingtalk_userid` 写入两条 `dingtalk_upload(status=pending)`，序号为 1、2。
5. 后台 worker 每 5 秒分别上传两条钉钉考勤流水；成功后将对应队列项更新为 `success` 并保存 `request_id`，失败则按退避策略独立重试。

签到接口返回 `dingtalkSync: pending` 表示两条钉钉流水都已进入上传队列。验收钉钉同步时应查询同一个 `checkin_id` 对应的两条 `dingtalk_upload` 记录，最终两条都必须为 `success`。

董峥宇当前已完成手机号映射，数据库中已保存钉钉 UserID，因此下一次签到会直接进入第 4 步的入队流程。
