// agent 独立上游配置的领域层测试。
//
// 这里只测【纯判定】，不碰仓储与 HTTP：Configured 与 Validate
// 是整条链路上唯一决定"独立上游算不算数"的地方，
// 而 agent 包的 resolveUpstream 与服务层的 resolveEndpointModel
// 都以它为准。这里松了，两处就会一起松。
package model

import (
	"strings"
	"testing"
)

func TestAgentEndpoint_Configured(t *testing.T) {
	cases := []struct {
		name string
		ep   *AgentEndpoint
		want bool
	}{
		{
			name: "地址与密钥都齐且已启用",
			ep: &AgentEndpoint{
				BaseURL: "https://api.example.com/v1", APIKey: "sk-x", Enabled: true,
			},
			want: true,
		},
		{
			name: "没启用",
			// 关掉 = 退回借用渠道，这是最保险的状态，也必须被判定为"不算数"。
			ep: &AgentEndpoint{
				BaseURL: "https://api.example.com/v1", APIKey: "sk-x", Enabled: false,
			},
			want: false,
		},
		{
			name: "只有地址没有密钥",
			// 这是最需要被挡住的一半配置：拿它去打上游必然 401，
			// 而站长看到的只是"助手不工作"，完全看不出少配了密钥。
			ep:   &AgentEndpoint{BaseURL: "https://api.example.com/v1", Enabled: true},
			want: false,
		},
		{
			name: "只有密钥没有地址",
			ep:   &AgentEndpoint{APIKey: "sk-x", Enabled: true},
			want: false,
		},
		{
			name: "地址与密钥都是空白字符",
			// 空白不算"填了"。站长在输入框里打了一格空格就以为配好了，
			// 是很常见的情形——不留 TrimSpace 的话它会被当成有效值。
			ep:   &AgentEndpoint{BaseURL: "   ", APIKey: "  ", Enabled: true},
			want: false,
		},
		{
			name: "未配置（空结构体）",
			ep:   &AgentEndpoint{},
			want: false,
		},
		{
			name: "仓储未接入（nil）",
			// Configured 挂在可能被调用的指针上，nil 必须安全返回 false，
			// 否则第一个部署里没接仓储的站点会直接崩在启动那一刻。
			ep:   nil,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ep.Configured(); got != tc.want {
				t.Fatalf("Configured() = %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestAgentEndpoint_Validate(t *testing.T) {
	cases := []struct {
		name string
		ep   *AgentEndpoint
		// wantErrSubstring 为空表示期望通过校验。
		wantErrSubstring string
	}{
		{
			name: "配齐了",
			ep: &AgentEndpoint{
				BaseURL: "https://api.example.com/v1", APIKey: "sk-x", Enabled: true,
				Kind: AgentEndpointOpenAI,
			},
		},
		{
			name: "未启用时什么都不校验",
			// 站长正常的操作顺序是"先填一半、最后打开开关"，
			// 这里拦下来只会让他以为自己填错了方向。
			ep: &AgentEndpoint{BaseURL: "https://api.example.com/v1"},
		},
		{
			name:             "启用了但两项都空",
			ep:               &AgentEndpoint{Enabled: true},
			wantErrSubstring: "同时填写",
		},
		{
			name:             "启用了只填地址",
			ep:               &AgentEndpoint{BaseURL: "https://x/v1", Enabled: true},
			wantErrSubstring: "还缺 API Key",
		},
		{
			name:             "启用了只填密钥",
			ep:               &AgentEndpoint{APIKey: "sk-x", Enabled: true},
			wantErrSubstring: "还缺接口地址",
		},
		{
			name: "地址有密钥留空但库里已有 → 通过",
			// 编辑页保存时密钥框必然是空的（不回显），
			// 那种空是"沿用原值"而不是"没配"，不能拦下来。
			ep: &AgentEndpoint{
				BaseURL: "https://x/v1", Enabled: true, APIKeySet: true,
			},
		},
		{
			name: "kind 为空按 OpenAI 处理",
			// 绝大多数第三方上游都是 OpenAI 兼容；
			// 这里若报错，前端不传 kind 就会全线失败。
			ep: &AgentEndpoint{
				BaseURL: "https://x/v1", APIKey: "sk-x", Enabled: true,
			},
		},
		{
			name: "kind 不受支持",
			ep: &AgentEndpoint{
				BaseURL: "https://x/v1", APIKey: "sk-x", Enabled: true,
				Kind: AgentEndpointKind("gemini"),
			},
			wantErrSubstring: "协议类型不受支持",
		},
		{
			name: "nil 不报错",
			ep:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.ep.Validate()
			if tc.wantErrSubstring == "" {
				if err != nil {
					t.Fatalf("期望通过校验，实际报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望报错（含 %q），实际通过", tc.wantErrSubstring)
			}
			// 断言的是"可执行的提示"而不是错误本身：
			// 站长看到"还缺 API Key"才知道该去补哪一栏，
			// 笼统的"配置无效"等于把判断责任推回给用户。
			if !strings.Contains(err.Error(), tc.wantErrSubstring) {
				t.Fatalf("报错文案不含 %q：%v", tc.wantErrSubstring, err)
			}
		})
	}
}

// TestAgentEndpoint_OrOpenAI 守住"空值不是错误"这条约定。
//
// 它在两处被依赖：仓储读取时把库里的空串归一为 OpenAI，
// 校验时放行未指定协议的配置。任何一处把它改成报错，
// 表现都会是"新建的配置保存不进去"。
func TestAgentEndpoint_OrOpenAI(t *testing.T) {
	cases := []struct {
		in   AgentEndpointKind
		want AgentEndpointKind
	}{
		{AgentEndpointOpenAI, AgentEndpointOpenAI},
		{AgentEndpointAnthropic, AgentEndpointAnthropic},
		{AgentEndpointKind(""), AgentEndpointOpenAI},
		{AgentEndpointKind("   "), AgentEndpointOpenAI},
		{AgentEndpointKind("不认识"), AgentEndpointOpenAI},
	}
	for _, tc := range cases {
		if got := tc.in.OrOpenAI(); got != tc.want {
			t.Errorf("OrOpenAI(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}
