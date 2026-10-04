// 本文件是 model.AgentEndpointRepository 的 SQL 实现。
//
// 意图（Why）：
//
//	让 agent 有一套自己的上游地址与密钥，而不是必须借用站点渠道。
//	好处有三（助手与业务流量不抢配额 / 可单独挑便宜模型 /
//	可指向一个支持工具调用的第三方上游），代价是要多维护一份凭据。
//
//	API Key 的加密方式与 SMTP 口令、渠道密钥完全一致
//	（internal/crypto 的 AES-GCM，密钥来自 AQUA_APP_KEY），
//	因此数据库随备份流出时，没有 APP_KEY 解不开。
//
// 流转（Flow）：
//
//	读取：Get → SELECT 单行 → cipher.Decrypt(api_key_cipher) → 领域对象
//	写入：Save → cipher.Encrypt(密钥) → 事务内 UPDATE，无行则 INSERT
//
// 扩展（Extend）：
//
//	新增字段时：在 model.AgentEndpoint 加字段 + 建迁移加列
//	+ 修改本文件的 agentEndpointColumns、Get 的 Scan 与 Save 的 SQL（三处必须同步）。
//	沿用 smtp_repo.go 的做法：先 UPDATE 再按需 INSERT，避免数据库方言差异
//	（SQLite 的 ON CONFLICT 与 MySQL 的 ON DUPLICATE KEY 关键字不同）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/crypto"
	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// agentEndpointColumns 是 agent_endpoint 的列清单（读写共用，避免两处手写导致错位）。
const agentEndpointColumns = `base_url, api_key_cipher, model, kind, enabled, updated_at`

// agentEndpointRepository 是 model.AgentEndpointRepository 的 SQL 实现。
//
// 并发安全：只持有 *sql.DB（自带连接池）与无状态的 *crypto.Cipher，可并发复用。
type agentEndpointRepository struct {
	db     *sql.DB
	cipher *crypto.Cipher
}

// NewAgentEndpointRepository 创建 agent 独立上游配置仓储。
//
// 参数 cipher 用于密钥加解密，不可为 nil ——
// 没有加密能力就不应该允许写入任何密钥，哪怕它属于站长自己。
func NewAgentEndpointRepository(db *sql.DB, cipher *crypto.Cipher) model.AgentEndpointRepository {
	return &agentEndpointRepository{db: db, cipher: cipher}
}

// Get 读取独立上游配置；从未保存过时返回空配置而非错误。
//
// 为什么返回空配置而不是 (nil, nil)（与 SMTP 仓储不同）：
//
//	本表的"未配置"不是异常状态而是常见状态（默认就是借用渠道），
//	而调用方每次建引擎都会问一次。让它处理 nil 只会让每个调用点
//	都写一遍判空，而漏掉一处就是一次空指针崩溃。空配置比 nil 安全。
func (r *agentEndpointRepository) Get(ctx context.Context) (*model.AgentEndpoint, error) {
	var (
		baseURL       string
		apiKeyCipher  string
		upstreamModel string
		kind          string
		enabled       int
		updatedAt     int64
	)
	query := "SELECT " + agentEndpointColumns + " FROM agent_endpoint WHERE id = 1"
	err := r.db.QueryRowContext(ctx, query).Scan(
		&baseURL, &apiKeyCipher, &upstreamModel, &kind, &enabled, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &model.AgentEndpoint{}, nil
		}
		return nil, fmt.Errorf("store: 读取 agent 独立上游配置失败: %w", err)
	}

	endpoint := &model.AgentEndpoint{
		BaseURL: baseURL,
		Model:   upstreamModel,
		// 空值按 OpenAI 兼容处理：绝大多数第三方上游都是这个协议，
		// 让站长必须显式选一次只会平白多一步。
		Kind:      model.AgentEndpointKind(strings.TrimSpace(kind)).OrOpenAI(),
		Enabled:   enabled == 1,
		APIKeySet: strings.TrimSpace(apiKeyCipher) != "",
	}

	if endpoint.APIKeySet {
		decrypted, err := r.cipher.Decrypt(apiKeyCipher)
		if err != nil {
			// 解不开时【不能】当"没配置"处理：那会让助手悄悄回退到借用渠道，
			// 而站长明明配过密钥、却在界面上看到"助手用的是别的渠道"，
			// 这种"配了等于没配"的现象极难排查。
			// 因此显式报错：界面上会显示"密钥解密失败，请重新填写"。
			return nil, fmt.Errorf(
				"store: agent 密钥解密失败（AQUA_APP_KEY 是否变更过？需重新填写密钥）: %w", err)
		}
		endpoint.APIKey = decrypted
	}
	return endpoint, nil
}

// Save 保存独立上游配置（单行 upsert）。
//
// 【APIKey 为空时必须沿用库中原有的密钥】
//
// 界面上密钥框不回显（安全要求），保存时必然是空的。
// 把这个空当成"清空"会让一次只改模型名的提交把密钥抹掉——
// 之后助手悄悄开始借用站点渠道，站长却毫无线索。
// 因此"沿用"是硬要求，不是优化。
//
// 实现上刻意【复用旧密文】而不是"解密→原样再加密"：
// AEAD 带随机 nonce，后者会让每次保存都产生一个不同的密文，
// 数据库在无意义的写入中不断变化（也让"这行有没有动过"变得没法判断）。
func (r *agentEndpointRepository) Save(ctx context.Context, endpoint *model.AgentEndpoint) error {
	if endpoint == nil {
		return errors.New("store: agent 独立上游配置不能为 nil")
	}

	// 读旧密文必须在事务开始之前完成：本方法的读与写分属两次数据库往返，
	// 混进同一事务只会让"事务开始时刻的旧值"与"UPDATE 命中的行"之间产生时序歧义。
	cipherText, err := r.resolveCipherText(ctx, endpoint)
	if err != nil {
		return err
	}

	enabled := 0
	if endpoint.Enabled {
		enabled = 1
	}
	kind := endpoint.Kind.OrOpenAI()
	now := time.Now().Unix()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启 agent 上游配置事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	update := `UPDATE agent_endpoint SET base_url = ?, api_key_cipher = ?, model = ?,
		kind = ?, enabled = ?, updated_at = ? WHERE id = 1`
	result, err := tx.ExecContext(ctx, update,
		strings.TrimSpace(endpoint.BaseURL), cipherText, strings.TrimSpace(endpoint.Model),
		string(kind), enabled, now)
	if err != nil {
		return fmt.Errorf("store: 更新 agent 上游配置失败: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取 agent 上游配置更新结果失败: %w", err)
	}
	if affected == 0 {
		insert := `INSERT INTO agent_endpoint (id, ` + agentEndpointColumns + `)
			VALUES (1, ?, ?, ?, ?, ?, ?)`
		if _, err := tx.ExecContext(ctx, insert,
			strings.TrimSpace(endpoint.BaseURL), cipherText, strings.TrimSpace(endpoint.Model),
			string(kind), enabled, now); err != nil {
			return fmt.Errorf("store: 写入 agent 上游配置失败: %w", err)
		}
	}

	return tx.Commit()
}

// resolveCipherText 决定本次写入的 api_key_cipher 列该放什么。
//
// 三种情况：
//   - 本次带了新密钥 → 加密后写入；
//   - 没带新密钥但库里已有 → 原样写回旧密文（沿用）；
//   - 没带新密钥且库里没有 → 空串（确实未配置）。
func (r *agentEndpointRepository) resolveCipherText(
	ctx context.Context,
	endpoint *model.AgentEndpoint,
) (string, error) {
	if plain := strings.TrimSpace(endpoint.APIKey); plain != "" {
		encrypted, err := r.cipher.Encrypt(plain)
		if err != nil {
			return "", fmt.Errorf("store: 加密 agent 密钥失败: %w", err)
		}
		return encrypted, nil
	}

	// 没有新密钥：读旧密文原样写回。
	// 查不到行 = 从未配置过 = 保持空，正确。
	var cipherText string
	err := r.db.QueryRowContext(ctx,
		"SELECT api_key_cipher FROM agent_endpoint WHERE id = 1").Scan(&cipherText)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// 真的读失败了不能当"没有旧密钥"——那会把密钥静默清空，
		// 而站长只在下次助手行为异常时才会发现。宁可让这次保存失败。
		return "", fmt.Errorf("store: 读取既有 agent 密钥失败: %w", err)
	}
	return cipherText, nil
}
