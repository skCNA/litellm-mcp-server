# LiteLLM MCP Server

通过 MCP Streamable HTTP 向 Claude Code、OpenClaw 等客户端提供 LiteLLM 团队成员及 key 预算查询和受控修改工具。默认只读，配置独立管理凭证后才能预览和执行预算修改。

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

### `query_team_key_budget`

按团队别名查询 LiteLLM 虚拟 key 的预算与消费。必填 `team_alias`，精确匹配；可选 `key_alias` 精确筛选。别名为空的 key 仍会出现在团队列表中。查询使用 `public.team_key_budget_view`，创建 SQL 见 [sql/team_key_budget_view.sql](sql/team_key_budget_view.sql)。

| 返回字段 | 说明 |
| --- | --- |
| `team_id`、`team_alias` | 所属团队 |
| `key_alias`、`key_name` | key 的显示名称；不包含 key/token 或其哈希 |
| `user_id` | 关联用户；团队级 key 可以为空 |
| `spend` | LiteLLM 记录的 key 消费；设置预算周期时对应当前周期 |
| `key_max_budget` | key 自身配置的额度 |
| `budget_table_max_budget` | key 关联预算记录中的额度 |
| `effective_max_budget` | 两种额度中较小的非空值；两者都为空时为 `null` |
| `budget_duration`、`budget_reset_at` | 预算周期与下次重置时间 |
| `expires`、`blocked`、`created_at` | key 过期、禁用与创建信息 |

默认每页返回 50 条，最多 100 条。通过 `page` 和 `page_size` 继续翻页；`count` 是当前页记录数，`has_more` 表示是否还有下一页。返回保留数据库空值，不会把未设置额度改成零。查询只读，服务账号只需对视图有 `SELECT` 权限；不要授予其读取 `LiteLLM_VerificationToken.token` 的权限。视图创建前应对照正在运行的 LiteLLM 数据库表结构核对字段。

### `preview_team_key_budget_change`

预览一个团队 key 的预算上限修改，不写入 LiteLLM。必须提供精确的 `team_alias`、`key_alias` 和正数 `max_budget`；别名必须在目标团队下唯一，LiteLLM 的 `/key/update` 也要求 key 别名能唯一定位。新额度不能超过管理员上限；不支持设为 0 或清除额度。

返回旧 key 自身额度、关联预算额度、旧生效额度、新 key 额度及预期生效额度。若关联预算低于新 key 额度，关联预算仍限制最终生效额度。预览有效期为 10 分钟。示例：

```json
{"team_alias":"math","key_alias":"batch-a","max_budget":50}
```

### `apply_team_key_budget_change`

先向管理员展示完整预览并取得明确确认，再调用：

```json
{"change_id":"替换为预览返回的编号","confirm":true}
```

执行时会重新核对目标和预算配置，只调用 LiteLLM `POST /key/update` 更新 `max_budget`，再从预算视图回读确认。不会更新 key 关联预算、消费、预算周期、重置时间或 TPM/RPM；同一编号不会重复提交。出现配置冲突或回读无法确认时，需要人工核查，不能自动创建新计划重试。服务会在预览和执行前检查部署的 `/openapi.json` 是否公开 `POST /key/update`；不支持该接口的部署会拒绝修改。


### `preview_member_budget_change`

生成有效期为 10 分钟的修改预览，不写入 LiteLLM。只处理已经有独立预算的现有普通成员（`role=user`），排除管理员及 `default_user_id`。

| 参数 | 必须 | 说明 |
| --- | --- | --- |
| `team_alias` | 是 | 精确匹配唯一团队；别名对应多个团队时拒绝 |
| `scope` | 是 | `member` 修改一个成员；`team_members` 修改团队现有普通成员 |
| `user_email` | 单成员模式 | 完整邮箱或完整邮箱前缀；匹配多个用户时拒绝；批量模式禁止提供 |
| `max_budget` | 是 | 每个目标成员的新额度，必须为有限正数，不超过管理员配置上限 |
| `reset_current_cycle_spend` | 否 | 布尔值，默认 `false`；`true` 表示修改额度时同时清零当前周期已消费金额 `spend`，不是将预算上限设为零 |

单成员示例：

```json
{"team_alias":"backend","scope":"member","user_email":"bob","max_budget":50}
```

整个团队普通成员示例：

```json
{"team_alias":"backend","scope":"team_members","max_budget":50}
```

修改单个成员额度并同时清零本周期消费：

```json
{"team_alias":"backend","scope":"member","user_email":"bob","max_budget":50,"reset_current_cycle_spend":true}
```

团队批量模式也支持这个开关，对预览确定的每位普通成员生效。省略或传 `false` 时完全保留原行为，不检查或调用清零接口；即使新旧额度相同，传 `true` 仍会执行清零。

返回 `change_id`、到期时间、冻结的目标名单、旧额度、新额度、当前消费、原周期、重置时间及清零选择；新额度不高于已消费金额时给出风险提示。每位成员的 `target_current_cycle_spend` 在选择清零时为 `0`，否则为 `null`。一次最多 100 位成员；超出限制时整体拒绝，不截断名单。批量中任一目标缺少独立预算、使用团队默认共享预算，或预算仍被其他实体引用，均不生成计划。

### `apply_member_budget_change`

先向管理员展示预览，获得明确确认后再调用：

```json
{"change_id":"替换为预览返回的编号","confirm":true}
```

仅接受预览编号和确认标记，不接受新的目标、额度或清零开关；清零选择必须在预览时确定并由管理员确认。预览首次执行前会检查过期时间和当前额度上限；逐成员重新验证普通成员身份、预算关联、共享引用以及预算配置。执行中的普通消费增长不构成配置冲突；本次预览之后加入团队的新成员不在目标名单内。

额度写入调用 LiteLLM `POST /team/member_update`，请求体只有 `team_id`、`user_id`、`max_budget_in_team`。选择清零时，先确认额度生效，再调用 `POST /team/{team_id}/member/{user_id}/reset_spend`，请求体固定为 `{"reset_to":0}`。不直接更新数据库；不清空累计消费 `total_spend`，不修改周期、重置时间、角色、团队总预算、用户全局预算或未来成员默认值；不提供零额度、无限额和清空预算上限操作。

返回的每个成员都有独立 `status`：

| 状态 | 含义 |
| --- | --- |
| `pending` | 仅生成预览，尚未提交 |
| `applied` | 新额度与原有其他配置已确认；未选择清零时消费计数未下降，选择清零时额外确认清零及累计消费未下降 |
| `unchanged` | 原额度相同，没有发送写请求 |
| `partial` | 额度已确认，但清零尚未提交，例如两步之间调用被取消或进程中断 |
| `conflict` | 配置、身份、关联已变化，或安全检查无法完成，没有提交该成员 |
| `unconfirmed` | 请求可能已经生效，但回读无法确认；必须人工核查 |
| `skipped` | 前项存在冲突、结果不确定、调用取消或进程中断，该成员未提交 |

每位成员同时返回 `budget_confirmed`（是否已确认额度）、`reset_status`（`not_requested`、`pending`、`skipped`、`confirmed` 或 `unconfirmed`）以及最近一次成功回读的 `observed_current_cycle_spend`。清零失败时额度可能已经生效，需要分别检查两项结果。

`state=done` 只表示这次执行已结束，不代表所有成员都成功。遇到冲突、部分完成或未确认结果会停止后续成员；已成功的操作不会自动回滚。额度更新与清零是两次 HTTP 操作，不具备原子性。

变更记录绑定管理令牌的摘要与 LiteLLM 地址。重复执行同一 `change_id` 返回持久化结果，不重复写入或清零；进程中断后，重试同一编号只回读可能已提交的操作，不继续尚未开始的清零或成员。清零成功回执会持久化，因此清零后正常新增消费不要求再次回到零。若回执丢失且回读消费不为零，则无法确认，不能推断为失败并重新清零。请求超时不等于写入失败；不要让模型通过新建计划自动重试 `partial` 或 `unconfirmed` 项，否则可能清掉后来新增的消费。

## 数据库初始化配置
创建 LiteLLM 预算视图
如果你使用的是 LiteLLM 自带的数据库，需要手动在 litellm 数据库中创建一个自定义视图，供服务查询团队成员的预算数据。

请在数据库中执行以下 SQL 语句：


```sql
CREATE OR REPLACE VIEW public.team_member_budget_view AS
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
    LEFT JOIN "LiteLLM_BudgetTable" b_member ON b_member.budget_id = tm.budget_id
)
LEFT JOIN "LiteLLM_BudgetTable" b_default
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
| `MCP_WRITE_AUTH_TOKEN` | 启用修改时 | 无 | 独立管理令牌，至少 32 字节，不能与只读令牌相同 |
| `LITELLM_URL` | 启用修改时 | 无 | LiteLLM 管理 API 基址，例如 `https://litellm.example.com`；禁止 URL 内置凭证 |
| `LITELLM_MASTER_KEY` | 启用修改时 | 无 | 服务端持有的 LiteLLM 管理密钥，不提供给模型 |
| `MCP_MAX_MEMBER_BUDGET` | 启用修改时 | 无 | 每位成员允许设置的最大额度，有限正数；单位与 LiteLLM 预算一致，通常为美元 |
| `MCP_MAX_KEY_BUDGET` | 否 | `MCP_MAX_MEMBER_BUDGET` | 每个 key 允许设置的最大额度，有限正数；未设置时沿用成员额度上限 |
| `MCP_CHANGE_DIR` | 启用修改时 | 无 | 保存预览及执行记录的绝对目录，权限须为 `0700` |

五个必填管理变量均不配置时保持原只读模式；只配置其中一部分会导致启动报错。`MCP_MAX_KEY_BUDGET` 是可选上限，单独设置时也会被视为不完整的管理配置。

为数据库连接创建只读账号。只查询时仅授予目标视图读取权限；启用修改时按下面的说明补充用于安全校验的只读权限，仍然不授予数据库写权限。不要把数据库密码、管理密钥或 MCP 令牌写入镜像、源码或 Git。

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

### 管理权限与兼容性

`MCP_AUTH_TOKEN` 只能查询，不能列出或调用四个管理工具。`MCP_WRITE_AUTH_TOKEN` 可查询并修改所有符合规则的团队；两种凭证使用独立 MCP 会话，不能借用管理会话绕过限制。此处没有实现按团队授权，也不能识别共享令牌背后的自然人；只向受信任管理员发放写令牌。

`confirm=true` 和工具描述不是人工审批系统。需要在 Claude/OpenClaw 客户端保留写工具审批，不要对执行工具开启自动批准。严格的多人审批需要另接身份认证及审批平台。

写接口及最小请求字段已按 [LiteLLM v1.93.0 源码](https://github.com/BerriAI/litellm/blob/v1.93.0/litellm/proxy/management_endpoints/team_endpoints.py) 和 [预算合并更新逻辑](https://github.com/BerriAI/litellm/blob/v1.93.0/litellm/proxy/management_endpoints/common_utils.py) 核对，但不代表生产版本已经验证。上线前务必在同版本测试实例确认：只发送额度时，不清零消费、不改变周期和重置时间。

定向清零按 [LiteLLM 主分支的成员接口源码](https://github.com/BerriAI/litellm/blob/main/litellm/proxy/management_endpoints/team_endpoints.py) 核对（2026-10-23）：只更新成员 `spend` 并失效相关缓存，不写 `total_spend` 或预算周期。此前核对的 v1.93.0 团队接口没有这条路由。选择清零时，预览和首次执行前均会读取部署的 `/openapi.json`，确认其公开 `POST /team/{team_id}/member/{user_id}/reset_spend`；缺少接口、模式文档不可读或格式异常时，在修改任何额度前报错。需要部署支持该接口的版本；不会通过修改预算到期时间、等待调度器或直接更新数据库来替代。

数据库与 `LITELLM_URL` 必须指向同一部署，使用主数据库，避免只读副本延迟导致误判。API 预算关联与数据库配置不一致时拒绝操作。缺少 `budget_reset_at` 等必要响应字段、缺少已核对版本的表结构或存在行级安全策略时也拒绝操作，不做兼容猜测。

共享检查覆盖团队默认预算、成员、虚拟密钥、终端客户、标签、组织、组织成员、项目，以及目录中发现的额外预算外键与 `public.LiteLLM_*` 表的 `budget_id` 列。数据库账号需要读取这些引用列及成员预算配置，授权示例见 [sql/budget_admin_read_permissions.sql](sql/budget_admin_read_permissions.sql)。仅授予密钥表的 `budget_id` 列权限，不需要读取密钥内容。

安全检查无法发现应用自行存放且没有外键、也不符合上述列规则的引用。如果你扩展了 LiteLLM 预算存储，先补充引用检查。预检与 HTTP 写入也不是一个数据库事务，外部管理员仍可能在两者之间修改关联；执行期间应避免其他入口并发调整相同预算。

## 本地运行

```bash
export DATABASE_URL='postgres://只读用户名:密码@localhost:5432/litellm?sslmode=disable'
export MCP_AUTH_TOKEN='替换为随机生成的令牌'
go run .
```

服务启动时会验证数据库连接。通过 `http://localhost:8080/mcp` 连接。

启用修改功能时，在已有查询配置基础上增加：

```bash
export LITELLM_URL='http://localhost:4000'
export LITELLM_MASTER_KEY='替换为测试实例管理密钥'
export MCP_WRITE_AUTH_TOKEN="$(openssl rand -hex 32)"
export MCP_MAX_MEMBER_BUDGET='1000'
export MCP_CHANGE_DIR="$PWD/.changes"
go run .
```

服务会创建缺失的记录目录。已有目录必须为 `0700`，记录文件为 `0600`；目录中包含邮箱与预算信息，禁止公开。更换写令牌或 LiteLLM 地址后，旧预览不再允许执行。

## Docker 部署

构建镜像：

```bash
 docker buildx build --platform linux/amd64 -t registry-ze.tencentcloudcr.com/devops/litellm-mcp-server:v6 .
docker push registry-ze.tencentcloudcr.com/devops/litellm-mcp-server:v6
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

启用管理能力时，预先设置上述管理环境变量，挂载持久化数据卷：

```bash
docker run --rm -p 8080:8080 \
  -e DATABASE_URL -e MCP_AUTH_TOKEN -e MCP_WRITE_AUTH_TOKEN \
  -e LITELLM_URL -e LITELLM_MASTER_KEY -e MCP_MAX_MEMBER_BUDGET \
  -e MCP_CHANGE_DIR=/data/changes \
  -v litellm-mcp-data:/data \
  litellm-mcp-server
```

镜像以 UID/GID `65532:65532` 运行，已准备 `/data/changes` 目录。绑定宿主目录时，应事先将目录所有者和 `0700` 权限配置为该用户可用。不要把容器内的 `localhost` 当作宿主机或另一个 LiteLLM 容器。

建议只部署一个管理实例；同一主机多进程必须使用同一个记录目录，文件锁才能串行执行。跨主机、多副本或不能可靠支持 `flock` 的网络文件系统不受支持，不能声称全局幂等。

保留持久化数据卷，容器重建不能删除它。记录含预览时间、执行时间、管理凭证摘要、目标、旧配置、新额度和逐项结果，但不是防篡改审计系统。按组织要求备份归档；未完成、`unconfirmed` 及仍需幂等查询的记录不要删除。归档或删除后，对应编号不可再查询，不会重新提交。

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

测试修改时，改用独立 `MCP_WRITE_AUTH_TOKEN`，应看到四个管理工具。修改成员预算先调用 `preview_member_budget_change`；修改 key 预算先调用 `preview_team_key_budget_change`。查看各自预览并人工确认后，分别调用对应的 `apply_...` 工具；更换令牌后重新建立 MCP 连接。

调用 `query_team_member_budget` 时，可以只传团队别名 `{"team_alias":"研发团队"}`、完整邮箱 `{"user_email":"bob@example.com"}` 或前缀 `{"user_email":"bob"}`，也可以组合查询：

```json
{"team_alias":"研发团队","user_email":"bob"}
```

修改工具定义后，重启服务并在 MCP 客户端中重新连接，以刷新工具的入参和返回结构。

新增自动测试使用模拟 LiteLLM HTTP 服务与数据库结果，覆盖最小写请求、预览不写入、身份歧义、共享引用、配置冲突、只读会话隔离、并发幂等、中断恢复、异常响应回读和部分成功；清零测试额外覆盖默认关闭、相同额度仍清零、缺少接口时提前拒绝、累计消费和周期保留、清零后新增消费、丢失回执和恢复时不重复清零。执行完整检查：

```bash
go test -race -timeout 60s ./...
go vet ./...
```

这些测试不连接真实 PostgreSQL，也不修改生产预算。上线验收还需在真实同版本测试环境核对数据库授权、视图、API 返回字段、周期和消费保留行为。只有本地假视图而没有 LiteLLM 基础表和管理 API 时，可以测试查询，不能完成预算修改联调。
