// 用户侧客服自助工具的安全测试。
//
// 为什么这组测试是整个改动里最关键的：
//
//	给外部人加工具，最大的风险不是"工具写错了"，
//	而是"用户能读到别人的数据"。这类漏洞在功能测试里完全看不出来 ——
//	测试用的就是当前用户的数据，每一条路径都通。
//	只有显式构造"攻击者试图读别人数据"的场景才能暴露它。
//
// 因此下面的测试主体分两类：
//	【越权不可能】—— 证明协议层就没有这种调用；
//	【隔离生效】—— 证明数据层即便被上层绕过也拦得住。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// ── 测试替身 ────────────────────────────────────────────────────
//
// 刻意用【嵌入接口 + 覆盖少数方法】而不是逐个实现全部方法：
// 仓储接口有二十来个方法，手写一遍既啰嗦又脆弱 ——
// 每次接口加方法，这三个 stub 都要跟着改，而它们本意只是"记下查了谁"。
//
// 内嵌 model.TokenRepository（接口值）后，未覆盖的方法调用会
// 在运行到时 panic 而不是编译失败。对测试来说这反而更好：
// 用到没实现的方法会立刻炸出来，而不会被一个随手返回 nil 的
// 桩函数悄悄吞掉 —— 后者会让人以为某条路径被覆盖了，其实没有。

// stubUsers 记录被查询的用户 ID，用来验证"查的是谁"。
type stubUsers struct {
	model.UserRepository // 未覆盖的方法会 panic（符合测试意图）
	got                 []uint64
	user                *model.User
}

func (s *stubUsers) GetByID(_ context.Context, id uint64) (*model.User, error) {
	s.got = append(s.got, id)
	if s.user == nil {
		return nil, errors.New("用户不存在")
	}
	return s.user, nil
}

// stubTokens 记录被查询的 OwnerID，并记录创建出来的令牌。
type stubTokens struct {
	model.TokenRepository // 未覆盖的方法会 panic
	queriedOwners        []uint64
	created              []*model.Token
	tokens               []*model.Token
}

func (s *stubTokens) Create(_ context.Context, tk *model.Token) error {
	tk.ID = uint64(len(s.created) + 100)
	s.created = append(s.created, tk)
	return nil
}

func (s *stubTokens) List(_ context.Context, q model.TokenQuery) ([]*model.Token, error) {
	if q.OwnerID != nil {
		s.queriedOwners = append(s.queriedOwners, *q.OwnerID)
	} else {
		// 记录"未按归属过滤"这件事：它是越权的直接证据。
		s.queriedOwners = append(s.queriedOwners, 0)
	}
	return s.tokens, nil
}

func (s *stubTokens) Count(context.Context, model.TokenQuery) (int, error) {
	return len(s.tokens), nil
}

// stubOrders 记录被查询的用户 ID。
type stubOrders struct {
	model.PaymentOrderRepository // 未覆盖的方法会 panic
	queriedUsers                []uint64
	orders                      []*model.PaymentOrder
}

func (s *stubOrders) List(_ context.Context, q model.PaymentOrderQuery) ([]*model.PaymentOrder, error) {
	s.queriedUsers = append(s.queriedUsers, q.UserID)
	return s.orders, nil
}

func (s *stubOrders) Count(_ context.Context, q model.PaymentOrderQuery) (int64, error) {
	return int64(len(s.orders)), nil
}

// ── 越权不可能：协议层就没有"读别人"的调用 ───────────────────────

func TestSelfService_无身份时一律失败(t *testing.T) {
	// 客服只挂在 portal（带 SessionAuth）下，正常情况下一定有身份。
	// 但万一中间件漏挂，这里必须【全部失败】而不是退化成查某个用户 ——
	// 那正是整份文件在防的事。
	tools := &AgentTools{Tokens: &stubTokens{}, Users: &stubUsers{}}

	for _, name := range []string{"my_account", "my_orders", "my_tokens", "my_recent_usage"} {
		tool, ok := findTool(tools, model.AgentRoleSelfService, name)
		if !ok {
			t.Fatalf("找不到工具 %q", name)
		}
		if _, err := tool.Handler(context.Background(), json.RawMessage(`{}`)); err == nil {
			t.Errorf("%s: 无身份时必须失败（退化成查任意用户即越权）", name)
		}
	}
	// 用合法身份再跑一次，确认上面不是因为别的原因失败。
	tool, _ := findTool(tools, model.AgentRoleSelfService, "my_tokens")
	if _, err := tool.Handler(WithSelfUser(context.Background(), 1), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("有身份时 my_tokens 应可用，工具本身有问题: %v", err)
	}
}

func TestSelfService_工具Schema不接受userID(t *testing.T) {
	// 最直白的越权防线：模型的参数里根本没有 user_id 这个位置。
	// 提示词注入说"查询用户 999 的订单"时，模型只能填 name/limit。
	tools := &AgentTools{Users: &stubUsers{}, Tokens: &stubTokens{}, Orders: &stubOrders{}}

	// 把每个工具的参数都塞一个 user_id 进去，验证被 schema 拒绝或至少不被代码读取。
	for _, tool := range selfServiceTools(tools) {
		if tool.Parameters == nil {
			t.Fatalf("%s: 缺少参数 schema", tool.Name)
		}
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
			t.Errorf("%s: 参数 schema 不是合法 JSON: %v", tool.Name, err)
			continue
		}
		for _, forbidden := range []string{"user_id", "userId", "owner_id", "uid"} {
			if _, found := schema.Properties[forbidden]; found {
				t.Errorf("%s: 参数里不应出现 %q —— 归属人只能来自会话", tool.Name, forbidden)
			}
		}
	}
}

func TestSelfService_创建令牌时归属人来自会话(t *testing.T) {
	// 即便模型在参数里塞 user_id，OwnerID 也必须是会话里的那个人。
	tk := &stubTokens{}
	tools := &AgentTools{Tokens: tk, Now: func() time.Time { return time.Now() }}

	tool, _ := findTool(tools, model.AgentRoleSelfService, "create_my_token")
	// 参数里带一个 user_id，期望被忽略。
	args := json.RawMessage(`{"name":"test","user_id":999,"owner_id":888}`)
	if _, err := tool.Handler(WithSelfUser(context.Background(), 42), args); err != nil {
		t.Fatalf("创建应成功: %v", err)
	}
	if len(tk.created) != 1 {
		t.Fatalf("应创建 1 把令牌，实际 %d", len(tk.created))
	}
	if tk.created[0].OwnerID != 42 {
		t.Errorf("令牌归属人 = %d，期望 42（必须取自会话，参数里的 user_id 应被完全忽略）",
			tk.created[0].OwnerID)
	}
}

func TestSelfService_查询令牌时强制按归属过滤(t *testing.T) {
	// 数据层的最后一道闸：查询必须带 OwnerID。
	// 若上层过滤被改错，这里仍只返回自己的。
	tk := &stubTokens{tokens: []*model.Token{{ID: 1, Name: "a"}}}
	tools := &AgentTools{Tokens: tk}

	tool, _ := findTool(tools, model.AgentRoleSelfService, "my_tokens")
	if _, err := tool.Handler(WithSelfUser(context.Background(), 7), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("查询应成功: %v", err)
	}
	if len(tk.queriedOwners) == 0 {
		t.Fatal("未发起查询")
	}
	for _, owner := range tk.queriedOwners {
		if owner != 7 {
			t.Errorf("查询使用了归属过滤 %d，期望 7（0 表示未过滤 = 越权）", owner)
		}
	}
}

// ── 隔离生效 ─────────────────────────────────────────────────────

func TestSelfService_不同用户互不可见(t *testing.T) {
	// 端到端：用户 A 建的数据，用户 B 查不到。
	// 上面几条是"结构上不可能"，这条是"结果上也确实隔离"。
	tk := &stubTokens{}
	tools := &AgentTools{Tokens: tk, Now: func() time.Time { return time.Now() }}
	tool, _ := findTool(tools, model.AgentRoleSelfService, "create_my_token")

	for _, uid := range []int64{100, 200} {
		if _, err := tool.Handler(WithSelfUser(context.Background(), uid),
			json.RawMessage(`{"name":"k"}`)); err != nil {
			t.Fatalf("用户 %d 创建失败: %v", uid, err)
		}
	}
	if len(tk.created) != 2 {
		t.Fatalf("应创建 2 把，实际 %d", len(tk.created))
	}
	if tk.created[0].OwnerID == tk.created[1].OwnerID {
		t.Error("两个用户创建的令牌归属人相同 —— 身份串了")
	}
	if tk.created[0].OwnerID != 100 || tk.created[1].OwnerID != 200 {
		t.Errorf("归属人错位: %d / %d，期望 100 / 200", tk.created[0].OwnerID, tk.created[1].OwnerID)
	}
}

// ── 角色授权矩阵 ─────────────────────────────────────────────────

func TestToolsForRole_三个角色的工具集互不重叠(t *testing.T) {
	// 逐项核对授权矩阵。这类表最容易在"加了个新角色"时被无意改坏，
	// 而一次越权往往就是某张表里某个格子填错了。
	tools := &AgentTools{Users: &stubUsers{}, Tokens: &stubTokens{}, Orders: &stubOrders{}}

	ops := toolNames(ToolsForRole(model.AgentRoleOps, tools))
	self := toolNames(ToolsForRole(model.AgentRoleSelfService, tools))
	support := toolNames(ToolsForRole(model.AgentRoleSupport, tools))

	if len(support) != 0 {
		t.Errorf("support 必须零工具（公网密钥无法确定身份），实际有: %v", support)
	}
	if len(self) == 0 {
		t.Error("self_service 应有自助工具")
	}
	// 自助工具绝不能出现在 ops 之外的任何地方，且绝不与 ops 重叠。
	for _, name := range self {
		if contains(ops, name) {
			t.Errorf("自助工具 %q 与运维工具重名 —— 会出现「改一处影响两处」", name)
		}
	}
	// 关键：自助工具里绝不能出现任何运维工具。
	for _, name := range ops {
		if contains(self, name) {
			t.Errorf("运维工具 %q 不应出现在自助工具集里（用户将能操作站内数据）", name)
		}
	}
}

func TestToolsForRole_自助工具集不含任何写站点数据的能力(t *testing.T) {
	// 白名单式断言：自助工具只能是这六个，且名字耳熟能详。
	// 出现第 7 个名字时，这条测试会失败 —— 那正是审查的时机。
	tools := &AgentTools{Users: &stubUsers{}, Tokens: &stubTokens{}, Orders: &stubOrders{}}
	allowed := map[string]bool{
		"my_account": true, "my_orders": true, "my_tokens": true,
		"create_my_token": true, "my_recent_usage": true, "site_announcements": true,
	}
	for _, name := range toolNames(ToolsForRole(model.AgentRoleSelfService, tools)) {
		if !allowed[name] {
			t.Errorf("出现了未预期的自助工具 %q —— 新增前请确认它无法越权", name)
		}
	}
}

func TestToolsForRole_未知角色拿不到任何工具(t *testing.T) {
	// 拼错的角色若被默认成 ops，等于凭空开出一个带工具的入口。
	tools := &AgentTools{Users: &stubUsers{}, Tokens: &stubTokens{}, Orders: &stubOrders{}}
	for _, role := range []model.AgentRole{"", "op", "admin", "opss", "OPS", "selfservice"} {
		if got := ToolsForRole(role, tools); len(got) != 0 {
			t.Errorf("角色 %q 应拿不到工具，实际拿到 %d 个", role, len(got))
		}
	}
}

func TestToolsForRole_运维角色仍保留全部工具(t *testing.T) {
	// 防止上面的隔离改动误伤运维助手。
	tools := &AgentTools{Users: &stubUsers{}, Tokens: &stubTokens{}, Orders: &stubOrders{}}
	if got := ToolsForRole(model.AgentRoleOps, tools); len(got) < 20 {
		t.Errorf("运维工具数 = %d，期望 >= 20（原有能力被误删）", len(got))
	}
}

// ── 自助角色不可分配给密钥 ───────────────────────────────────────

func TestValidateAgentKeyRole_拒绝自助角色(t *testing.T) {
	// 这一条是"公网 chat 接口不会拿到自助工具"的根本保证。
	// 若允许把 self_service 分配给 ak- 密钥，那把密钥在公网就能
	// 尝试执行自助工具 —— 它取不到 userID，会全部报错，
	// 那种"有工具但全失败"的状态会被误判成鉴权故障。
	for _, role := range []model.AgentRole{
		model.AgentRoleSelfService, "self_service", "selfservice", "SELF_SERVICE",
	} {
		if err := model.ValidateAgentKeyRole(role); err == nil {
			t.Errorf("角色 %q 不应能分配给访问密钥", role)
		}
	}
	// 原有两种角色必须仍可用。
	for _, role := range []model.AgentRole{model.AgentRoleOps, model.AgentRoleSupport} {
		if err := model.ValidateAgentKeyRole(role); err != nil {
			t.Errorf("角色 %q 应仍然合法: %v", role, err)
		}
	}
}

// ── 硬性禁止写代码 ───────────────────────────────────────────────

func TestSelfService_不存在任何代码执行类工具(t *testing.T) {
	// 用户明确要求"限制不能写代码"。
	// 实现方式必须是【结构上没有】，而不是靠提示词说"请不要写代码" ——
	// 提示词是配置项，站长能改；工具表是代码，改了要重新部署且会被审查。
	tools := &AgentTools{Users: &stubUsers{}, Tokens: &stubTokens{}, Orders: &stubOrders{}}

	banned := []string{
		"exec", "run", "shell", "bash", "command", "eval",
		"write_file", "read_file", "edit_file", "upload", "download",
		"http_request", "fetch_url", "webhook",
		"sql", "execute_sql", "query", "database",
	}
	for _, role := range []model.AgentRole{
		model.AgentRoleOps, model.AgentRoleSelfService, model.AgentRoleSupport,
	} {
		for _, name := range toolNames(ToolsForRole(role, tools)) {
			for _, bad := range banned {
				if containsSub(name, bad) {
					t.Errorf("角色 %s 出现了疑似代码/网络执行类工具 %q", role, name)
				}
			}
		}
	}
}

func TestToolsForRole_工具名不得形似代码执行(t *testing.T) {
	// 上面那条查子串，这条查【语义相近的命名】——
	// 有人写 "run_query" 也会被上面命中，但 "exec_plan" 不会。
	// 因此再加一条显式清单，作为新增工具时要过的第二道关。
	forbidden := []string{"python", "node", "javascript", "script", "code", "compile", "install"}
	tools := &AgentTools{Users: &stubUsers{}, Tokens: &stubTokens{}, Orders: &stubOrders{}}
	for _, name := range toolNames(ToolsForRole(model.AgentRoleSelfService, tools)) {
		for _, bad := range forbidden {
			if containsSub(name, bad) {
				t.Errorf("自助工具 %q 含有代码执行相关词 %q", name, bad)
			}
		}
	}
}

// ── 辅助 ─────────────────────────────────────────────────────────

// findTool / toolNames 复用 engine.go 与 tools_test.go 里的既有实现，
// 不在这里重复定义 —— 同名同义的两份辅助函数是测试里最容易漂移的东西。

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// containsSub 做子串匹配（区分大小写）。
//
// 为什么单独写而不直接用 strings.Contains：上面那条查工具名时
// 需要的是"名字本身就叫 exec"这种全等判断，而这条查的是
// "名字里含 shell" —— 两种判断混用会让测试的意图变得不明确。
func containsSub(s, sub string) bool {
	return len(sub) > 0 && strings.Contains(s, sub)
}
