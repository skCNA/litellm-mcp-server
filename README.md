# LiteLLM MCP Server

通过 MCP Streamable HTTP 向 Claude Code、OpenClaw 等客户端提供 LiteLLM 团队成员预算查询工具。

## 提供的工具

### `query_team_member_budget`

按团队别名 `team_alias` 和/或邮箱前缀 `user_email` 查询 PostgreSQL 关系 `public.team_member_budget_view`，返回该视图中的完整费用与预算数据：

| 返回字段 | 说明 |
| --- | --- |
| `team_id` | 团队 ID |
| `team_alias` | 团队别名 |
| `user_id` | 用户 ID |
| `user_email` | 用户邮箱 |
| `member_current_cycle_spend` | 成员本周期费用 |
| `member_total_spend` | 成员累计费用 |
| `member_max_budget` | 成员预算上限 |
| `budget_source` | 预算来源 |

工具支持以下两个入参：

| 参数 | 说明 |
| --- | --- |
| `team_alias` | 团队别名，精确匹配；单独提供时返回该团队所有成员的完整记录 |
| `user_email` | 完整邮箱或邮箱前缀；忽略域名，按 `@` 前的完整内容区分大小写精确匹配，返回各域名和团队下的匹配记录 |

两个入参至少提供一个；同时提供时使用 AND 条件，返回同时匹配团队别名和邮箱前缀的记录。参数会去除首尾空白，每个原始参数最多 255 字节；缺少有效条件或参数超长会返回错误。邮箱参数不允许空前缀、空域名、多个 `@`、内部空白或控制字符；例如 `@example.com`、`bob@` 和 `bob@@example.com` 都会报错，即使同时提供了团队别名也不会忽略错误的邮箱条件。

例如，传入 `{"user_email":"bob@company.com"}` 或 `{"user_email":"bob"}` 均可匹配 `bob@example.com`、`bob@other.com`，不会匹配 `bob1@example.com`、`bobby@example.com` 或 `Bob@example.com`。这里的“前缀”指整个邮箱本地部分，不是字符串开头的模糊匹配；`%`、`_`、`+` 等字符不会作为通配符或别名规则处理。

响应中的 `records` 包含所有匹配记录，每条包含上述 8 个字段；`count` 为返回记录数。没有匹配时返回 `{"records":[],"count":0}`。数据库中的 `NULL` 保持为 JSON `null`，不会转换成零。

查询不再按 200 条截断，也不再返回 `truncated` 字段。保留 10 秒查询超时，查询失败或超时时返回错误，不返回部分数据作为完整结果。大量记录会增加响应体积和模型上下文消耗。

“完整数据”仅指此视图中的字段和记录，不包含其他表的数据。成员预算上限不是团队总预算，不应将成员预算简单相加后作为团队总预算。同一别名对应多个团队或同一邮箱前缀对应多个用户时，会返回所有匹配记录，可通过返回的 ID 区分；结果仍保留数据库中的完整邮箱，不会改写或合并记录。邮箱前缀唯一是业务约定，不是数据库保证，不能用于身份认证或权限判断。

邮箱筛选使用参数化条件 `split_part(user_email, '@', 1) = $1`。数据量较大时，可在视图对应的底层表评估相同表达式的索引；普通邮箱列索引不一定适用于此表达式，不能直接在普通视图上创建索引。


## 数据库初始化配置
创建 LiteLLM 预算视图
如果你使用的是 LiteLLM 自带的数据库，需要手动在 litellm 数据库中创建一个自定义视图，供服务查询团队成员的预算数据。

请在数据库中执行以下 SQL 语句：


```shell
CREATE OR REPLACE VIEW litellm.team_member_budget_view AS
SELECT
    t.team_id,
    t.team_alias,
    tm.user_id,
    u.user_email,
    tm.spend AS member_current_cycle_spend,
    tm.total_spend AS member_total_spend,
    COALESCE(b_member.max_budget, b_default.max_budget) AS member_max_budget,
    CASE
        WHEN b_member.max_budget IS NOT NULL THEN 'member_budget'::text
        WHEN b_default.max_budget IS NOT NULL THEN 'team_default_member_budget'::text
        ELSE 'unlimited_or_not_set'::text
    END AS budget_source
FROM (
    (
        (
            "LiteLLM_TeamMembership" tm
            JOIN "LiteLLM_TeamTable" t ON t.team_id = tm.team_id
        )
        LEFT JOIN "LiteLLM_UserTable" u ON u.user_id = tm.user_id
    )
    LEFT JOIN "LiteLLM_BudgetTable"* b_member ON b_member.budget_id = tm.budget_id
)
LEFT JOIN "LiteLLM_BudgetTable70" b_default
    ON b_default.budget_id = (t.metadata ->> 'team_member_budget_id'::text)
WHERE COALESCE(tm.user_id, ''::text) <> 'default_user_id'::text;

```


## 配置

服务通过环境变量读取配置：

| 变量 | 必须 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `DATABASE_URL` | 是 | 无 | PostgreSQL 连接串 |
| `MCP_AUTH_TOKEN` | 是 | 无 | Bearer 令牌，至少 32 字节 |
| `MCP_ADDR` | 否 | `:8080` | HTTP 监听地址 |

为数据库连接创建只读账号，并仅授予它读取目标视图的权限。不要把数据库密码或 MCP 令牌写入镜像、源码或 Git。

MCP Bearer 令牌是服务级凭证：持有该令牌的客户端可以按工具参数查询视图中的任意用户或团队数据。若不同调用者应有不同的数据范围，需要在服务端加入与调用者身份绑定的授权规则；仅要求模型传入用户 ID 不能构成访问控制。

```sql
GRANT USAGE ON SCHEMA public TO mcp_readonly;
GRANT SELECT ON public.team_member_budget_view TO mcp_readonly;
```

生成随机访问令牌（64 个十六进制字符）：

```bash
openssl rand -hex 32
```

MCP HTTP 地址为 `/mcp`。对外部署时，应使用 HTTPS 反向代理或受保护的内网，并将访问令牌通过 `Authorization: Bearer <令牌>` 请求头发送。容器内服务本身提供 HTTP。

## 本地运行

```bash
export DATABASE_URL='postgres://只读用户名:密码@localhost:5432/litellm?sslmode=disable'
export MCP_AUTH_TOKEN='替换为随机生成的令牌'
go run .
```

服务启动时会验证数据库连接。通过 `http://localhost:8080/mcp` 连接。

## Docker 部署

构建镜像：

```bash
docker build -t litellm-mcp-server .
```

运行容器：

```bash
docker run --rm \
  -p 8080:8080 \
  -e DATABASE_URL \
  -e MCP_AUTH_TOKEN \
  litellm-mcp-server
```

运行命令前，在当前终端或容器编排平台中设置这两个环境变量。生产环境建议通过 Secret 注入敏感变量，并在反向代理上配置 TLS。数据库地址必须能从容器网络访问。

## 连接 MCP 客户端

### Claude Code

```bash
claude mcp add --transport http litellm https://你的域名/mcp \
  --header "Authorization: Bearer 你的令牌"
```

配置完成后运行 `/mcp` 检查连接状态。

### OpenClaw

在 OpenClaw 的 **Settings → MCP** 中添加远程服务器，传输方式选择 **Streamable HTTP**，地址填写 `https://你的域名/mcp`，并设置请求头：

```text
Authorization: Bearer 你的令牌
```

## 测试

运行单元测试和静态检查：

```bash
go test ./...
go vet ./...
```

可用 MCP Inspector 测试本地服务。先按“本地运行”启动服务，再在另一个终端启动 Inspector：

```bash
npx -y @modelcontextprotocol/inspector
```

在 Inspector 中选择 Streamable HTTP，地址填写 `http://localhost:8080/mcp`，并添加 `Authorization: Bearer <MCP_AUTH_TOKEN>` 请求头。Inspector 终端不需要数据库连接串。

调用 `query_team_member_budget` 时，可以只传团队别名 `{"team_alias":"研发团队"}`、完整邮箱 `{"user_email":"bob@example.com"}` 或前缀 `{"user_email":"bob"}`，也可以组合查询：

```json
{"team_alias":"研发团队","user_email":"bob"}
```

修改工具定义后，重启服务并在 MCP 客户端中重新连接，以刷新工具的入参和返回结构。
