package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	stdsync "sync"
	"time"

	"github.com/arkpix/relay/internal/common"
	"github.com/arkpix/relay/internal/crypto"
)

// tombstoneRetentionMs 墓碑保留期（§7.2：90 天）。超过后墓碑可清理、
// 客户端游标视为过期，需全量重建（409 SYNC_FULL_REQUIRED）。
const tombstoneRetentionMs = int64(90 * 24 * time.Hour / time.Millisecond)

// maxPushItems 单域单次 push 上限（§7.2：500 条）。
const maxPushItems = 500

// Service 同步服务。所有查询按 account_id 隔离。
type Service struct {
	db     *sql.DB
	tokens *TokenStore
	enc    *crypto.Cipher // 静态加密（§9）；nil = 不加密
	now    func() int64   // 可注入时间源（测试用）
	pushMu stdsync.Mutex
}

// NewService 创建同步服务并恢复各域 token 游标。enc 为数据静态加密器（§9，
// AES-256-GCM，nil 不加密）；存量明文与加密新数据可混存（按 enc:v1: 前缀区分）。
func NewService(ctx context.Context, db *sql.DB, enc *crypto.Cipher) (*Service, error) {
	ts := NewTokenStore()
	if err := ts.Load(ctx, db); err != nil {
		return nil, err
	}
	return &Service{
		db:     db,
		tokens: ts,
		enc:    enc,
		now:    func() int64 { return time.Now().UnixMilli() },
	}, nil
}

// PushItem push 条目。data 落库前经 enc 加密（启用时），库内为密文文本。
type PushItem struct {
	Key       string          `json:"key"`
	Data      json.RawMessage `json:"data"`
	UpdatedAt int64           `json:"updatedAt"`
	Deleted   bool            `json:"deleted"`
}

// PushRequest POST /sync/v1/push 请求体（§7.2）。
type PushRequest struct {
	Domain    string     `json:"domain"`
	BaseToken string     `json:"baseToken"`
	Items     []PushItem `json:"items"`
}

// PushResponse push 响应。conflicts v1 恒为空数组（服务端权威 LWW，无冲突上抛）。
type PushResponse struct {
	Accepted  int    `json:"accepted"`
	SyncToken string `json:"syncToken"`
	Conflicts []any  `json:"conflicts"`
}

// PullItem pull 响应条目（含墓碑 deleted:true）。
type PullItem struct {
	Key       string          `json:"key"`
	Data      json.RawMessage `json:"data"`
	UpdatedAt int64           `json:"updatedAt"`
	Deleted   bool            `json:"deleted"`
}

// PullResponse GET /sync/v1/pull 响应（§7.2）。
type PullResponse struct {
	Items     []PullItem `json:"items"`
	SyncToken string     `json:"syncToken"`
	HasMore   bool       `json:"hasMore"`
}

// Push 合并一批条目：域校验 → baseToken 保留期检查 → LWW 幂等合并 → token 前进。
// pushMu 串行化整个「读旧值-分配 seq-写入-token 前进」临界区，保证并发下同域
// token 严格单调且 LWW 合并不交错（SQLite 单写者，串行开销可忽略）。
func (s *Service) Push(ctx context.Context, accountID int64, req *PushRequest) (*PushResponse, error) {
	if !validDomain(req.Domain) {
		return nil, common.BadRequest("unknown domain")
	}
	if len(req.Items) > maxPushItems {
		return nil, common.BadRequest("too many items: max 500 per push")
	}
	if err := validateItems(req.Domain, req.Items); err != nil {
		return nil, err
	}
	nowMs := s.now()

	// baseToken 检查（§7.2）：与当前一致或为空（首次）直接接受；不一致时
	// 落后未超 90 天保留期也接受——服务端权威，LWW 幂等合并天然容忍旧 baseToken。
	if cur := s.tokens.Current(accountID, req.Domain); req.BaseToken != "" && req.BaseToken != cur {
		baseMs, err := ParseToken(req.BaseToken)
		if err != nil {
			return nil, common.BadRequest("invalid baseToken")
		}
		if nowMs-baseMs > tombstoneRetentionMs {
			return nil, common.NewError(http.StatusConflict, "SYNC_FULL_REQUIRED",
				"base token beyond retention window; full resync required")
		}
	}

	s.pushMu.Lock()
	defer s.pushMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin push tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// LWW：逐条与库中现有 updated_at 比较，已有 >= 传入者跳过（幂等）。
	accepted := make([]PushItem, 0, len(req.Items))
	var maxUpdatedAt int64
	for _, it := range req.Items {
		var existAt int64
		err := tx.QueryRowContext(ctx,
			`SELECT updated_at FROM sync_entries WHERE account_id = ? AND domain = ? AND "key" = ?`,
			accountID, req.Domain, it.Key).Scan(&existAt)
		switch {
		case err == nil && existAt >= it.UpdatedAt:
			continue // 旧写后到，跳过
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("read sync entry: %w", err)
		}
		accepted = append(accepted, it)
		if it.UpdatedAt > maxUpdatedAt {
			maxUpdatedAt = it.UpdatedAt
		}
	}

	seqBase, token := s.tokens.Next(accountID, req.Domain, len(accepted), nowMs)
	for i, it := range accepted {
		del := 0
		if it.Deleted {
			del = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO sync_entries (account_id, domain, "key", data, updated_at, deleted, seq)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			accountID, req.Domain, it.Key, s.enc.Encrypt(string(it.Data)), it.UpdatedAt, del, seqBase+int64(i)); err != nil {
			return nil, fmt.Errorf("upsert sync entry: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sync_domains (account_id, domain, sync_token, latest_updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(account_id, domain) DO UPDATE SET
		   sync_token = excluded.sync_token,
		   latest_updated_at = MAX(latest_updated_at, excluded.latest_updated_at)`,
		accountID, req.Domain, token, maxUpdatedAt); err != nil {
		return nil, fmt.Errorf("update sync domain: %w", err)
	}
	// 墓碑清理策略（择一，§7.2 保留 90 天）：惰性清理——push 时顺手删该域过期墓碑。
	// 过期判定用服务端 seq（≈写入毫秒，单调递增）而非客户端可控的 updated_at，
	// 防止伪造旧 updatedAt 绕过保留期（PLAN §8 的“tombstone 时间”由 seq 兼任）。
	// pull 不主动过滤过期墓碑，依赖 push 路径回收；纯只读域的过期墓碑会多留一段时间，
	// 语义无害（客户端按 deleted:true 处理即可）。
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM sync_entries WHERE account_id = ? AND domain = ? AND deleted = 1 AND seq < ?",
		accountID, req.Domain, nowMs-tombstoneRetentionMs); err != nil {
		return nil, fmt.Errorf("prune tombstones: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit push tx: %w", err)
	}

	return &PushResponse{Accepted: len(accepted), SyncToken: token, Conflicts: []any{}}, nil
}

// Pull 拉取条目：since 为空串全量；否则按 seq 水位增量。返回含墓碑。
// hasMore 时 syncToken 为最后一个返回条目的 seq 派生的临时续页 token（格式同源）。
func (s *Service) Pull(ctx context.Context, accountID int64, domain, since string, limit int) (*PullResponse, error) {
	if !validDomain(domain) {
		return nil, common.BadRequest("unknown domain")
	}
	sinceSeq := int64(-1)
	if since != "" {
		ms, err := ParseToken(since)
		if err != nil || s.now()-ms > tombstoneRetentionMs {
			// since 无效或过旧超保留期：客户端清本地后全量 pull（§7.2）。
			return nil, common.NewError(http.StatusConflict, "SYNC_FULL_REQUIRED",
				"since token invalid or expired; full resync required")
		}
		sinceSeq = ms
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT "key", data, updated_at, deleted, seq FROM sync_entries
		 WHERE account_id = ? AND domain = ? AND seq > ?
		 ORDER BY seq ASC LIMIT ?`,
		accountID, domain, sinceSeq, limit+1)
	if err != nil {
		return nil, fmt.Errorf("query sync entries: %w", err)
	}
	defer rows.Close()

	items := make([]PullItem, 0, limit)
	seqs := make([]int64, 0, limit)
	for rows.Next() {
		var it PullItem
		var data string
		var del, seq int64
		if err := rows.Scan(&it.Key, &data, &it.UpdatedAt, &del, &seq); err != nil {
			return nil, fmt.Errorf("scan sync entry: %w", err)
		}
		plain, err := s.enc.Decrypt(data) // 密文按 enc:v1: 前缀解密，明文原样（§9 混存兼容）
		if err != nil {
			return nil, fmt.Errorf("decrypt sync entry: %w", err)
		}
		it.Data = json.RawMessage(plain)
		it.Deleted = del != 0
		items = append(items, it)
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sync entries: %w", err)
	}

	hasMore := len(items) > limit
	token := s.tokens.Current(accountID, domain)
	if hasMore {
		items = items[:limit]
		// 续页：token 数值部分 = 本页末条 seq，客户端回传即续拉。
		token = fmt.Sprintf("st_%d_%s", seqs[limit-1], randHex6())
	}
	return &PullResponse{Items: items, SyncToken: token, HasMore: hasMore}, nil
}

// validateItems 校验整批条目：key 必填；data 须为 JSON 对象（缺省归一为 {}）；
// 敏感字段防御（§7.2）；bookmark_snapshot 结构校验（§7.3，墓碑放宽）。
func validateItems(domain string, items []PushItem) error {
	for i := range items {
		it := &items[i]
		if it.Key == "" {
			return common.BadRequest("missing field: items[].key")
		}
		raw := it.Data
		if len(raw) == 0 || string(raw) == "null" {
			raw = json.RawMessage("{}")
		}
		var obj any
		if err := json.Unmarshal(raw, &obj); err != nil {
			return common.BadRequest("items[].data must be valid JSON")
		}
		m, ok := obj.(map[string]any)
		if !ok {
			return common.BadRequest("items[].data must be a JSON object")
		}
		if containsSensitiveField(m) {
			return common.NewError(http.StatusBadRequest, "SENSITIVE_FIELD_REJECTED",
				"data contains sensitive field name (*token*)")
		}
		if domain == DomainBookmarkSnapshot && !it.Deleted && !validateBookmarkSnapshot(m) {
			return common.BadRequest("invalid bookmark_snapshot: illustId required, imageUrls must be string array")
		}
		it.Data = raw
	}
	return nil
}
