-- 由数据库管理员检查 LiteLLM 部署版本的表结构后执行。
-- 视图仅暴露 key 的标识信息和预算信息，不暴露 token 或 token 哈希。
CREATE OR REPLACE VIEW public.team_key_budget_view AS
SELECT
    t.team_id,
    t.team_alias,
    vt.key_alias,
    vt.key_name,
    vt.user_id,
    vt.spend,
    vt.max_budget AS key_max_budget,
    b.max_budget AS budget_table_max_budget,
    CASE
        WHEN vt.max_budget IS NULL THEN b.max_budget
        WHEN b.max_budget IS NULL THEN vt.max_budget
        ELSE LEAST(vt.max_budget, b.max_budget)
    END AS effective_max_budget,
    COALESCE(vt.budget_duration, b.budget_duration) AS budget_duration,
    COALESCE(vt.budget_reset_at, b.budget_reset_at) AS budget_reset_at,
    vt.expires,
    vt.blocked,
    vt.created_at
FROM public."LiteLLM_VerificationToken" vt
JOIN public."LiteLLM_TeamTable" t
    ON t.team_id = vt.team_id
LEFT JOIN public."LiteLLM_BudgetTable" b
    ON b.budget_id = vt.budget_id;

GRANT SELECT ON public.team_key_budget_view TO mcp_readonly;
