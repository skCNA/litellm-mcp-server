-- 由数据库管理员检查后执行；只增加读取权限，不授予数据库写权限。
-- mcp_readonly 应为服务专用角色，连接到与 LITELLM_URL 同一部署的主数据库。
GRANT USAGE ON SCHEMA public TO mcp_readonly;
GRANT SELECT ON public.team_member_budget_view TO mcp_readonly;
GRANT SELECT ON public."LiteLLM_BudgetTable" TO mcp_readonly;
GRANT SELECT (team_id, user_id, budget_id) ON public."LiteLLM_TeamMembership" TO mcp_readonly;
GRANT SELECT (team_id, metadata, members_with_roles) ON public."LiteLLM_TeamTable" TO mcp_readonly;
GRANT SELECT (budget_id) ON public."LiteLLM_VerificationToken" TO mcp_readonly;
GRANT SELECT (budget_id) ON public."LiteLLM_EndUserTable" TO mcp_readonly;
GRANT SELECT (budget_id) ON public."LiteLLM_TagTable" TO mcp_readonly;
GRANT SELECT (budget_id) ON public."LiteLLM_OrganizationTable" TO mcp_readonly;
GRANT SELECT (budget_id) ON public."LiteLLM_OrganizationMembership" TO mcp_readonly;
GRANT SELECT (budget_id) ON public."LiteLLM_ProjectTable" TO mcp_readonly;

-- 若安全检查发现新增外键表或其他 LiteLLM_* 表的 budget_id 列，
-- 需人工审查后补充对应模式的 USAGE 与引用列 SELECT 权限。
-- 不要为了绕过检查而给服务授予超级用户、BYPASSRLS 或数据库写权限。
