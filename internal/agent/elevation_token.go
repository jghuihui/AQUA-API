// 提权令牌的生成与摘要。
//
// 意图（Why）：
//
//	单独成文件而不是塞进 elevation.go，是因为这两个函数是【原语】——
//	它们不涉及提权语义，只提供"造随机串"和"算摘要"。
//	把原语与策略混在一个文件里，改策略时容易顺手改到原语，
//	而原语一旦出错（比如换成 math/rand）是静默且致命的。
//
// 与 API key 的差别（不可照抄）：
//
//	API key 要长期存库、可能被跨进程比对，所以前缀 + SHA-256。
//	提权令牌只活 5 分钟、只在本进程内存里比对，因此用更快的 SHA-256 即可，
//	不需要 bcrypt —— bcrypt 那点开销在这里没有任何理由付出。
//	但【必须】是密码学随机源：令牌是提权的唯一凭据，
//	可预测的令牌等于没有提权。
package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/LTZY-ACU/ltzy-api/internal/crypto"
)

// elevationTokenBytes 是令牌随机字节数。
//
// 取 32 字节（256 位）：足够长到无法被穷举或从内存布局反推，
// 又短到不进日志、不截断。提权令牌 5 分钟就作废，
// 不需要 API key 那样的寿命。
const elevationTokenBytes = 32

// newOpaqueToken 生成一枚提权令牌。
//
// 失败时 panic 而不是返回 error：这是 crypto/rand 读不到随机源，
// 属于进程级的不健康状态（容器熵池未初始化等）。
// 那种情况下继续跑下去会签发可预测的令牌 —— 那比直接崩溃危险得多。
// 站点启动时不需要访问网络，因此这里不会因为外部依赖而失败。
func newOpaqueToken() string {
	buf := make([]byte, elevationTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("agent: 读取密码学随机源失败，提权机制不可用: %v", err))
	}
	return "elev-" + hex.EncodeToString(buf)
}

// hashOpaqueToken 计算令牌摘要。
//
// 存摘要而非明文的理由与 API key 相同：
// 内存 dump 出来的记录无法直接用于提权。
func hashOpaqueToken(plain string) string {
	return crypto.SHA256Hex(plain)
}
