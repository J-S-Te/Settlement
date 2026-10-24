package runtime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	core "github.com/J-S-Te/license-core"
)

// State 仅用于诊断；状态只能通过签名快照，
// 或厂商独立签名的一次性时钟恢复令牌更新。
type State struct {
	Protocol              int      `json:"protocol"`
	SnapshotJWS           string   `json:"snapshotJWS"`
	ControlRevision       uint64   `json:"controlRevision"`
	HighestLicenseVersion uint64   `json:"highestLicenseVersion"`
	EnforcementStarted    bool     `json:"enforcementStarted"`
	Enforced              bool     `json:"enforced"`
	ClockHighWater        int64    `json:"clockHighWater"`
	ClockBlocked          bool     `json:"clockBlocked"`
	ConsumedRecoveryIDs   []string `json:"consumedRecoveryIDs"`
}

const stateFields = "protocol snapshotJWS controlRevision highestLicenseVersion enforcementStarted enforced clockHighWater clockBlocked consumedRecoveryIDs"

type FileRuntime struct {
	path         string
	binding      Binding
	platformKeys map[string]ed25519.PublicKey
	vendorKeys   map[string]ed25519.PublicKey
}

// NewFileRuntime 不会创建或初始化已授权状态。父目录须由服务部署负责持有和保护，
// 所有使用该目录的服务进程
// 必须采用相同的绑定与信任配置。
func NewFileRuntime(path string, binding Binding, platformKeys, vendorKeys map[string]ed25519.PublicKey) (*FileRuntime, error) {
	if path == "" || !validBinding(binding) || len(platformKeys) == 0 || len(vendorKeys) == 0 {
		return nil, ErrInvalid
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	copyKeys := func(keys map[string]ed25519.PublicKey) (map[string]ed25519.PublicKey, error) {
		out := make(map[string]ed25519.PublicKey, len(keys))
		for kid, key := range keys {
			if !identifier(kid) || len(key) != ed25519.PublicKeySize {
				return nil, ErrInvalid
			}
			out[kid] = append(ed25519.PublicKey(nil), key...)
		}
		return out, nil
	}
	pk, err := copyKeys(platformKeys)
	if err != nil {
		return nil, err
	}
	vk, err := copyKeys(vendorKeys)
	if err != nil {
		return nil, err
	}
	for _, platformKey := range pk {
		for _, vendorKey := range vk {
			if bytes.Equal(platformKey, vendorKey) {
				return nil, ErrInvalid
			}
		}
	}
	return &FileRuntime{absolute, binding, pk, vk}, nil
}

func (r *FileRuntime) withLock(ctx context.Context, action func() error) (result error) {
	if ctx == nil {
		return ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0700); err != nil {
		return fmt.Errorf("%w: directory: %v", ErrState, err)
	}
	f, err := lock(ctx, r.path+".lock")
	if err != nil {
		return fmt.Errorf("%w: lock: %w", ErrState, err)
	}
	defer func() {
		if err := unlock(f); err != nil {
			result = errors.Join(result, fmt.Errorf("%w: unlock: %v", ErrState, err))
		}
	}()
	return action()
}

func (r *FileRuntime) load() (State, PlatformSnapshot, error) {
	var state State
	f, err := openNoFollow(r.path, syscall.O_RDONLY)
	if err != nil {
		return state, PlatformSnapshot{}, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxBytes {
		f.Close()
		return state, PlatformSnapshot{}, ErrInvalid
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, maxBytes+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return state, PlatformSnapshot{}, errors.Join(readErr, closeErr)
	}
	if err := strict(raw, &state, stateFields); err != nil {
		return state, PlatformSnapshot{}, err
	}
	snapshot, err := VerifySnapshot(state.SnapshotJWS, r.binding, r.platformKeys, r.vendorKeys)
	if err != nil {
		return state, PlatformSnapshot{}, err
	}
	if state.Protocol != Protocol || state.ControlRevision != snapshot.Revision || state.HighestLicenseVersion != snapshot.HighestLicenseVersion || !validTime(state.ClockHighWater) || state.EnforcementStarted != (rank(snapshot.EnforcementState) >= 1) || state.Enforced != (snapshot.EnforcementState == Enforced) || len(state.ConsumedRecoveryIDs) > 1024 {
		return state, PlatformSnapshot{}, ErrInvalid
	}
	seen := map[string]bool{}
	for _, id := range state.ConsumedRecoveryIDs {
		if !identifier(id) || seen[id] {
			return state, PlatformSnapshot{}, ErrInvalid
		}
		seen[id] = true
	}
	return state, snapshot, nil
}

func (r *FileRuntime) save(state State) (result error) {
	raw, err := json.Marshal(state)
	if err != nil || len(raw) > maxBytes {
		return ErrState
	}
	dir := filepath.Dir(r.path)
	f, err := os.CreateTemp(dir, ".license-state-*")
	if err != nil {
		return fmt.Errorf("%w: temporary state: %v", ErrState, err)
	}
	temp := f.Name()
	closed := false
	defer func() {
		if !closed {
			result = errors.Join(result, f.Close())
		}
		if err := os.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}()
	if err := f.Chmod(0600); err != nil {
		return fmt.Errorf("%w: permissions: %v", ErrState, err)
	}
	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("%w: write: %v", ErrState, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("%w: sync: %v", ErrState, err)
	}
	if err := f.Close(); err != nil {
		closed = true
		return fmt.Errorf("%w: close: %v", ErrState, err)
	}
	closed = true
	if err := os.Rename(temp, r.path); err != nil {
		return fmt.Errorf("%w: replace: %v", ErrState, err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("%w: parent directory: %v", ErrState, err)
	}
	err = errors.Join(d.Sync(), d.Close())
	if err != nil {
		return fmt.Errorf("%w: parent sync: %v", ErrState, err)
	}
	return nil
}

// observe 在返回时钟回拨错误前持久化阻断标记。
// 再次调快时钟不会清除该标记。
func (r *FileRuntime) observe(state *State, now int64) error {
	if !validTime(now) {
		return ErrInvalid
	}
	if state.ClockBlocked {
		return ErrClock
	}
	if state.ClockHighWater-now > 300 {
		state.ClockBlocked = true
		if err := r.save(*state); err != nil {
			return err
		}
		return ErrClock
	}
	if now > state.ClockHighWater {
		state.ClockHighWater = now
		return r.save(*state)
	}
	return nil
}

// ApplySnapshot 原子地更新控制状态，不负责网络同步。
// 拒绝旧修订版以及同一修订号对应不同内容的快照。
// 允许重放已持久化的完全相同签名字节，以重试 ACK。
func (r *FileRuntime) ApplySnapshot(ctx context.Context, raw string, now time.Time) error {
	next, err := VerifySnapshot(raw, r.binding, r.platformKeys, r.vendorKeys)
	if err != nil {
		return err
	}
	if !validTime(now.Unix()) || next.SnapshotIssuedAt > now.Unix()+300 {
		return ErrInvalid
	}
	return r.withLock(ctx, func() error {
		state, old, err := r.load()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: load: %v", ErrState, err)
		}
		if err == nil {
			if err := r.observe(&state, now.Unix()); err != nil {
				return err
			}
			if next.Revision == state.ControlRevision && raw == state.SnapshotJWS {
				// observe 已持久化推进时钟高水位，或因已锁存的回拨状态拒绝操作。
				// 授权日期与修订号均保持不变。
				return nil
			}
			if next.Revision <= state.ControlRevision || next.HighestLicenseVersion < state.HighestLicenseVersion || rank(next.EnforcementState) < rank(old.EnforcementState) || next.SnapshotIssuedAt < old.SnapshotIssuedAt {
				return ErrRollback
			}
			if err := r.validateAdvance(old, next, state.HighestLicenseVersion); err != nil {
				return err
			}
		} else {
			state = State{Protocol: Protocol, ClockHighWater: now.Unix(), ConsumedRecoveryIDs: []string{}}
		}
		state.SnapshotJWS = raw
		state.ControlRevision = next.Revision
		state.HighestLicenseVersion = next.HighestLicenseVersion
		state.EnforcementStarted = rank(next.EnforcementState) >= 1
		state.Enforced = next.EnforcementState == Enforced
		return r.save(state)
	})
}

func (r *FileRuntime) validateAdvance(old, next PlatformSnapshot, highest uint64) error {
	oc, op, err := licenses(old, r.vendorKeys)
	if err != nil {
		return err
	}
	nc, np, err := licenses(next, r.vendorKeys)
	if err != nil {
		return err
	}
	known := map[uint64]string{}
	if oc != nil {
		known[oc.Version] = old.CurrentLicenseJWS
	}
	if op != nil {
		known[op.Version] = old.PendingLicenseJWS
	}
	for _, pair := range []struct {
		license *core.License
		raw     string
	}{{nc, next.CurrentLicenseJWS}, {np, next.PendingLicenseJWS}} {
		if pair.license == nil {
			continue
		}
		if original, ok := known[pair.license.Version]; ok {
			if original != pair.raw {
				return ErrRollback
			}
		} else if pair.license.Version <= highest {
			return ErrRollback
		}
		for _, previous := range []*core.License{oc, op} {
			if previous != nil && (previous.CustomerID != pair.license.CustomerID || previous.ProductID != pair.license.ProductID) {
				return ErrRollback
			}
		}
	}
	if oc != nil && (nc == nil || nc.Version < oc.Version) {
		return ErrRollback
	}
	// 已观察到的待生效整证替换不得消失，
	// 也不得被较低版本替代；转为生效时保留原始签名令牌。
	if op != nil && !(nc != nil && nc.Version >= op.Version || np != nil && np.Version >= op.Version) {
		return ErrRollback
	}
	return nil
}

// Evaluate 按签名中的绝对有效期判定，同步失败或缺失不会延长有效期。
// 具有迁移资格的签名 PENDING_ENFORCEMENT 快照，
// 仅在从未导入商业授权时保留旧版行为。
func (r *FileRuntime) Evaluate(ctx context.Context, operation core.Operation, now time.Time) error {
	switch operation {
	case core.ESSENTIAL_SERVICE:
		return nil
	case core.READ_HISTORY, core.EXPORT_HISTORY, core.MUTATE_BUSINESS:
	default:
		return core.ErrDenied
	}
	return r.withLock(ctx, func() error {
		state, snapshot, err := r.load()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrState, err)
		}
		if err := r.observe(&state, now.Unix()); err != nil {
			return err
		}
		// 即使时钟抖动在容忍范围内，也不得重新激活已到期授权，
		// 或撤销已生效的待替换整证。
		effectiveNow := now
		if state.ClockHighWater > now.Unix() {
			effectiveNow = time.Unix(state.ClockHighWater, 0)
		}
		// 迁移资格仅用于识别旧安装，不豁免签名授权的范围或绝对有效期检查。
		// 一旦已观察到授权版本，
		// 回退检查就必须阻止重新进入无授权兼容模式。
		if snapshot.EnforcementState == Pending && snapshot.MigrationEligible && !state.EnforcementStarted && state.HighestLicenseVersion == 0 && snapshot.CurrentLicenseJWS == "" && snapshot.PendingLicenseJWS == "" {
			return nil
		}
		current, pending, err := licenses(snapshot, r.vendorKeys)
		if err != nil {
			return err
		}
		selected := current
		if pending != nil && effectiveNow.Unix() >= pending.NotBefore {
			selected = pending
		}
		if selected == nil {
			return core.ErrDenied
		}
		return core.Evaluate(*selected, r.binding.Application, operation, effectiveNow, r.binding.InstanceID, r.binding.Environment)
	})
}

func (r *FileRuntime) ReadState(ctx context.Context) (State, error) {
	var state State
	err := r.withLock(ctx, func() error {
		loaded, _, err := r.load()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrState, err)
		}
		state = loaded
		return nil
	})
	return state, err
}

// RecoverClock 仅接受厂商独立签名、绑定最高已观察授权版本的恢复凭证，
// 并在同一持久化事务内核销凭证 ID。
// 不提供布尔开关重置或未签名的恢复路径。
func (r *FileRuntime) RecoverClock(ctx context.Context, raw string, now time.Time) error {
	recovery, err := core.VerifyRecovery(raw, r.vendorKeys)
	if err != nil {
		return err
	}
	if !validTime(now.Unix()) {
		return ErrInvalid
	}
	return r.withLock(ctx, func() error {
		state, snapshot, err := r.load()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrState, err)
		}
		if !state.ClockBlocked {
			return ErrInvalid
		}
		current, pending, err := licenses(snapshot, r.vendorKeys)
		if err != nil {
			return err
		}
		latest := current
		if pending != nil {
			latest = pending
		}
		if latest == nil || latest.Version != state.HighestLicenseVersion {
			return core.ErrDenied
		}
		if err := core.ValidateRecovery(recovery, *latest, now); err != nil {
			return err
		}
		if now.Unix() < recovery.AnchorAt-300 || now.Unix() > recovery.AnchorAt+300 {
			return ErrClock
		}
		if len(state.ConsumedRecoveryIDs) >= 1024 {
			return ErrState
		}
		for _, id := range state.ConsumedRecoveryIDs {
			if id == recovery.ID {
				return ErrRollback
			}
		}
		state.ConsumedRecoveryIDs = append(state.ConsumedRecoveryIDs, recovery.ID)
		state.ClockHighWater = recovery.AnchorAt
		if now.Unix() > state.ClockHighWater {
			state.ClockHighWater = now.Unix()
		}
		state.ClockBlocked = false
		return r.save(state)
	})
}
