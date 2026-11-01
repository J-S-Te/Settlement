package platform

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/j-s-te/settlement/internal/service"
	central "github.com/j-s-te/settlement/internal/sessiongovernance"
)

var ErrSessionUnavailable = errors.New("settlement session service is temporarily unavailable")

type centralSession struct {
	TenantID, IdentityID, CentralID, Subject, SID       string
	BindingCipher, PrincipalJSON, TokenCipher, IDCipher []byte
}
type centralQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// centralExpectation 固定结算与平台的租户、客户端和环境信任边界。
// 参数：无。
// 返回值：中央响应的精确范围。
func (a *Authenticator) centralExpectation() central.Expectation {
	return central.Expectation{TenantID: a.cfg.OIDCTenantID, ClientID: a.cfg.OIDCClientID, ApplicationCode: "settlement", EnvironmentCode: a.cfg.OIDCEnvironmentCode}
}

// loadCentralSession 精确读取当前Cookie的服务器记录，不再使用本地last_seen驱动空闲期限。
// 参数：ctx为请求，query为连接或事务，hash为Cookie摘要，lock为是否加行锁。
// 返回值：本地密文记录；不存在返回未认证，数据库故障返回依赖不可用。
func loadCentralSession(ctx context.Context, query centralQuerier, hash []byte, lock bool) (centralSession, error) {
	statement := `SELECT tenant_id,identity_id,principal_json,oauth_token_ciphertext,id_token_ciphertext,
	 central_binding_ciphertext,COALESCE(central_session_id,''),COALESCE(oidc_subject,''),COALESCE(oidc_sid,'')
	 FROM settlement_oidc_session WHERE session_id_hash=? AND revoked_at IS NULL AND session_expires_at>UTC_TIMESTAMP(3)`
	if lock {
		statement += " FOR UPDATE"
	}
	var row centralSession
	err := query.QueryRowContext(ctx, statement, hash).Scan(&row.TenantID, &row.IdentityID, &row.PrincipalJSON, &row.TokenCipher, &row.IDCipher, &row.BindingCipher, &row.CentralID, &row.Subject, &row.SID)
	if errors.Is(err, sql.ErrNoRows) {
		return row, ErrUnauthenticated
	}
	if err != nil {
		return row, ErrSessionUnavailable
	}
	return row, nil
}

// readCentral 验证精确绑定的中央周期，普通请求不会推进活动或固定截止。
// 参数：ctx为请求，row为密文记录，interact表示通过同源保护的真实活动。
// 返回值：非敏感窗口；旧记录、失效及依赖故障保持不同错误语义。
func (a *Authenticator) readCentral(ctx context.Context, row centralSession, interact bool) (central.Context, error) {
	if len(row.BindingCipher) == 0 {
		return central.Context{}, ErrUnauthenticated
	}
	secret, err := a.decrypt(row.BindingCipher)
	if err != nil || len(secret) == 0 {
		return central.Context{}, ErrUnauthenticated
	}
	var state central.Context
	if interact {
		state, err = a.central.Activity(ctx, string(secret))
	} else {
		state, err = a.central.State(ctx, string(secret))
	}
	if err == nil {
		err = central.Validate(state, a.centralExpectation(), row.IdentityID)
	}
	if errors.Is(err, central.ErrInactive) || errors.Is(err, central.ErrBindingConflict) {
		return central.Context{}, ErrUnauthenticated
	}
	if err != nil {
		return central.Context{}, ErrSessionUnavailable
	}
	if state.SessionID != row.CentralID {
		return central.Context{}, ErrUnauthenticated
	}
	return state, nil
}

// authenticateCentral 在中央有效的前提下串行刷新及复核结算当前授权。
// 参数：ctx为请求，r携带当前结算HttpOnly Cookie。
// 返回值：结算业务主体；确定失效仅撤销当前会话，网络或数据库故障保留记录。
func (a *Authenticator) authenticateCentral(ctx context.Context, r *http.Request) (service.Principal, error) {
	cookie, err := r.Cookie(a.cfg.OIDCSessionCookieName)
	if err != nil || cookie.Value == "" {
		return service.Principal{}, ErrUnauthenticated
	}
	hash := digest(cookie.Value)
	row, err := loadCentralSession(ctx, a.db, hash, false)
	if err != nil {
		return service.Principal{}, err
	}
	if row.TenantID != a.cfg.OIDCTenantID {
		return service.Principal{}, ErrUnauthenticated
	}
	if _, err = a.readCentral(ctx, row, false); err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			if _, revokeErr := a.db.ExecContext(ctx, "UPDATE settlement_oidc_session SET revoked_at=UTC_TIMESTAMP(3) WHERE session_id_hash=? AND revoked_at IS NULL", hash); revokeErr != nil {
				return service.Principal{}, ErrSessionUnavailable
			}
		}
		return service.Principal{}, err
	}
	rotationCtx, cancel := central.RotationContext(ctx)
	err = a.rotateCentralSession(rotationCtx, hash)
	cancel()
	if err != nil {
		return service.Principal{}, err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return service.Principal{}, ErrSessionUnavailable
	}
	defer tx.Rollback()
	row, err = loadCentralSession(ctx, tx, hash, true)
	if err != nil {
		return service.Principal{}, err
	}
	if _, err = a.readCentral(ctx, row, false); err != nil {
		return a.centralFailure(ctx, tx, hash, err)
	}
	current, updated, err := a.authorizeCentralCredentials(ctx, row)
	if err != nil {
		return a.centralFailure(ctx, tx, hash, err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_oidc_session SET principal_json=?,oauth_token_ciphertext=?,id_token_ciphertext=?,
	 authorization_revision=?,authorization_checked_at=UTC_TIMESTAMP(3),token_expires_at=?,last_seen_at=UTC_TIMESTAMP(3)
	 WHERE session_id_hash=? AND revoked_at IS NULL`, mustJSON(current), updated.TokenCipher, updated.IDCipher, current.AuthorizationRevision, oidcSQLTime(updated.TokenExpiresAt), hash); err != nil {
		return service.Principal{}, ErrSessionUnavailable
	}
	if err = tx.Commit(); err != nil {
		return service.Principal{}, ErrSessionUnavailable
	}
	return current, nil
}

// rotateCentralSession 在第一事务串行刷新并落盘，后续权限故障或切页不丢单次refresh。
// 参数：ctx为最多15秒的已验证轮换上下文，hash为本Cookie摘要。
// 返回值：保存结果；确定失效仅撤销当前行，其他错误不伪造成功。
func (a *Authenticator) rotateCentralSession(ctx context.Context, hash []byte) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrSessionUnavailable
	}
	defer tx.Rollback()
	row, err := loadCentralSession(ctx, tx, hash, true)
	if err != nil {
		return err
	}
	if _, err = a.readCentral(ctx, row, false); err != nil {
		_, err = a.centralFailure(ctx, tx, hash, err)
		return err
	}
	updated, changed, err := a.rotateCentralCredentials(ctx, row)
	if err != nil {
		_, err = a.centralFailure(ctx, tx, hash, err)
		return err
	}
	if changed {
		_, err = tx.ExecContext(ctx, `UPDATE settlement_oidc_session
		 SET oauth_token_ciphertext=?,id_token_ciphertext=?,token_expires_at=?,authorization_checked_at=?
		 WHERE session_id_hash=? AND revoked_at IS NULL`, updated.TokenCipher, updated.IDCipher,
			oidcSQLTime(updated.TokenExpiresAt), time.Unix(1, 0).UTC(), hash)
		if err != nil {
			return ErrSessionUnavailable
		}
	}
	if tx.Commit() != nil {
		return ErrSessionUnavailable
	}
	return nil
}

// centralFailure 只在明确失效时提交当前行撤销，依赖异常保持事务回滚。
// 参数：ctx为请求，tx为持锁事务，hash为当前Cookie摘要，cause为刷新或中央校验错误。
// 返回值：空主体及准确错误；撤销写回失败保持依赖不可用。
func (a *Authenticator) centralFailure(ctx context.Context, tx *sql.Tx, hash []byte, cause error) (service.Principal, error) {
	if !errors.Is(cause, ErrUnauthenticated) {
		return service.Principal{}, ErrSessionUnavailable
	}
	if _, err := tx.ExecContext(ctx, "UPDATE settlement_oidc_session SET revoked_at=UTC_TIMESTAMP(3) WHERE session_id_hash=?", hash); err != nil {
		return service.Principal{}, ErrSessionUnavailable
	}
	if err := tx.Commit(); err != nil {
		return service.Principal{}, ErrSessionUnavailable
	}
	return service.Principal{}, ErrUnauthenticated
}

type refreshedCentralSession struct {
	centralSession
	TokenExpiresAt time.Time
}
