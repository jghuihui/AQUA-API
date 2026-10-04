-- 迁移 0056：agent 独立上游配置
--
-- 背景（为什么需要它）：
--   agent 此前只能"借用"站点渠道来调用模型。这带来三个实际问题，
--   对【单站长自部署】的站点尤其明显：
--     1) 助手与业务流量抢同一批配额。渠道被限流时，先挂的是助手——
--        而助手的对话一轮要发多次请求，最容易被限流打断。
--     2) 站长想给助手单独挑一个便宜/快的模型（问答任务用不着旗舰模型），
--        但改渠道会影响所有业务流量。
--     3) 站长可能只有一个第三方上游，它不支持工具调用（tool_calls），
--        而运维 agent 的价值就在工具调用上。借渠道时若碰上个不支持的
--        上游，表现为"助手能聊天但一查数据就失败"，极难定位。
--   给 agent 一套独立的 base_url + api_key，上述三个问题一起消失，
--   且不必为此新建一个正式渠道（新建渠道意味着它会出现在模型广场、
--   参与路由、参与健康巡检，还要额外交代模型清单）。
--
-- 风险控制（与 0023_smtp_settings 完全同构，三条缺一不可）：
--   1) 密钥【加密落库】：api_key_cipher 存的是用 AQUA_APP_KEY 派生的
--      AES-GCM 密文（见 internal/crypto），与渠道密钥、支付密钥、SMTP 口令
--      同一套做法。数据库随备份流出时，没有 APP_KEY 解不开。
--   2) 接口【绝不回传密钥】：后台接口只返回 api_key_set（是否已配置），
--      永不返回明文或密文；前端只能"重填"不能"查看"。
--   3) 【留空 = 沿用原值】：编辑页保存时密钥输入框必然是空的（因为不回显），
--      若"未传即清空"，一次只改模型名的提交就会把密钥抹掉。
--      这一点在 handler 里由指针字段 + "空串沿用"双重保障。
--
-- 表设计：单行表（id 恒为 1），与 smtp_settings 同构。
--   为什么不用 KV 设置表：本条是一组【互相依赖】的参数
--   （base_url 与 api_key 必须成对存在才算配置完整），拆成设置键后
--   "算不算配好了"这个判定要散落在多处，而那正是最容易出错的判定。
--   单行表让"配没配好"变成一次原子读取。
--   CHECK (id = 1) 从数据层杜绝第二行（多行会让"用哪一行"变得不确定）。
--
-- enabled 的语义是【是否启用独立上游】，不是【agent 总开关】：
--   关掉它 = 退回"借用站点渠道"，这是最保险的状态，也是首次部署的默认。
--   这样站长可以先试独立上游，不满意一键切回，而不必清空配置重来。

CREATE TABLE IF NOT EXISTS agent_endpoint (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    base_url       TEXT    NOT NULL DEFAULT '',   -- 上游地址，如 https://api.example.com/v1
    api_key_cipher TEXT    NOT NULL DEFAULT '',   -- API Key 的 AES-GCM 密文（空串=未配置）
    model          TEXT    NOT NULL DEFAULT '',   -- 默认模型名；留空则沿用「运行配置」里的模型
    kind           TEXT    NOT NULL DEFAULT 'openai', -- 协议类型：openai（OpenAI 兼容）/ anthropic
    enabled        INTEGER NOT NULL DEFAULT 0,    -- 1=用独立上游；0=借用站点渠道
    updated_at     INTEGER NOT NULL DEFAULT 0     -- 最后更新时间（Unix 秒）
);
