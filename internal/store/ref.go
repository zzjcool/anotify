package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 本文件是 P2.2 的**引用层**：link 的一端不存 capability_versions.id，而存一个引用。
//
// 引用写法（设计 typed-pipeline §3.2）：
//
//	analyze        跟随 latest —— 升级自动生效
//	analyze@1      钉死那一版 —— 渐进发布 / 隔离 / 回滚
//
// 为什么不能存 id：id 里嵌了 content_hash8，且**每一版一个 id**。升级之后
// 「原来那条 link」还指着旧 id，于是升级永远不生效；反过来，想让升级生效就得
// 让派发去猜「这个 id 对应的名字的最新版是谁」——那等于把引用语义藏进两处。
// 存引用之后，「是哪一版」只剩一个问题，答案只看引用本身。
//
// 解析只有一处（本文件的 ResolveRef）：route 的 plan 与 store 的配置期校验都走它，
// 否则「保存时说指向 v3、派发时却解析到 v4」这种分歧无法自圆。

// RefVersionSep 是引用里「名字」与「版本」的分隔符（设计 §3.2 的 `name@version`）。
const RefVersionSep = '@'

// FormatRef 把 (name, version) 拼成**钉死**引用（`name@version`）。
//
// 迁移 27 用它把存量 id 重写成钉死引用（保住「升级不动线」的既有行为），
// 入口边之外的所有既有 link 都是这个形态。
func FormatRef(name string, version int) string {
	return name + string(RefVersionSep) + strconv.Itoa(version)
}

// SplitRef 把引用拆成 (名字, 版本, 是否钉死)。
//
// 不钉死时 version=0、pinned=false，name=原字符串。
//
// 为什么从**最后**一个 '@' 拆：能力名里允许出现 '@'（仓库没有对名字做字符集限制），
// 而版本号一定是纯数字。只有「最后一个 '@' 之后是正整数」才当成钉死引用，
// 否则整串就是名字——这样 `weird@name` 仍然是一个合法的名字引用，
// 不会因为拼写习惯被静默解析到别的地方去。
func SplitRef(ref string) (name string, version int, pinned bool) {
	ref = strings.TrimSpace(ref)
	i := strings.LastIndexByte(ref, RefVersionSep)
	if i <= 0 || i == len(ref)-1 {
		return ref, 0, false
	}
	v, err := strconv.Atoi(ref[i+1:])
	if err != nil || v <= 0 {
		return ref, 0, false
	}
	return ref[:i], v, true
}

// RefName 返回引用的名字部分（链路环检测/图快照只关心名字）。
func RefName(ref string) string {
	name, _, _ := SplitRef(ref)
	return name
}

// RefMatches 判定引用是否指向 (name, version)：名字必须相同，
// 钉死引用还要求版本相同（跟随 latest 的引用对任何版本都算「同一个能力」）。
//
// 用途：预案体检（「这条边装了吗」）与图快照比对——判断的是**意图**是否一致，
// 不是「此刻解析到同一版」。
func RefMatches(ref, name string, version int) bool {
	gotName, gotVer, pinned := SplitRef(ref)
	if gotName != name {
		return false
	}
	return !pinned || gotVer == version
}

// ErrRefUnresolved 表示**引用本身就不成立**（空串 / 空名字）：这是调用方的形状错误，
// 不是「查不到」。
//
// 与 ErrNotFound 的分工：ResolveRef 查不到名字/版本时返回 ErrNotFound（那是数据事实，
// handler 翻 404、配置期翻 ErrEdgeRejected）；只有「引用是空的」这种明显写错的情况
// 才走这个哨兵，文案里能立刻看出是参数问题。
var ErrRefUnresolved = errors.New("store: link ref unresolved")

// GetCapabilityLatest 取某个名字的**最高版本**（= latest）；未命中返回 ErrNotFound。
//
// 这是「跟随 latest」这条引用语义的唯一实现：`name` 型 link 每次派发都调它，
// 所以升级（新版本写入）之后下一次派发就解析到新版本——这正是 P2.2 要的效果。
func (d *DB) GetCapabilityLatest(ctx context.Context, namespaceID, name string) (*CapabilityVersion, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	return d.scanCapability(ctx,
		`SELECT `+capabilityColumns+` FROM capability_versions
		 WHERE namespace_id = ? AND name = ? ORDER BY version DESC LIMIT 1`, namespaceID, name)
}

// ResolveRef 把引用解析成能力定义。
//
//   - `name`       → 该名字的 latest（每次派发重新解析 = 升级自动生效）
//   - `name@ver`   → 那一版（钉死 = 升级不影响）
//
// 解析不到返回 ErrNotFound（handler 翻 404，配置期翻 ErrEdgeRejected）。
func (d *DB) ResolveRef(ctx context.Context, namespaceID, ref string) (*CapabilityVersion, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	name, version, pinned := SplitRef(ref)
	if name == "" {
		return nil, fmt.Errorf("%w: empty ref", ErrRefUnresolved)
	}
	if !pinned {
		return d.GetCapabilityLatest(ctx, namespaceID, name)
	}
	return d.GetCapability(ctx, namespaceID, name, version)
}

// resolveLinkRef 把「用户给的字符串」归一成要落库的引用，并返回它解析到的定义。
//
// 三种输入，一条**唯一**的归一规则：
//
//	`name`         → 原样保存（跟随 latest），校验它现在能解析
//	`name@version` → 原样保存（钉死），校验那一版存在
//	能力 id         → 转成 `name@version`（**输入兼容**，绝不把 id 存进 ref 列）
//
// id 那条是给「P2.2 之前的客户端 / e2e」留的一个版本：它们只会传
// `capability_versions.id`。这也是**唯一**会往库里写钉死引用的旧路径——
// 用户的意图本来就是「就是这个 id 那一版」，钉死正好等价。
//
// 解析不到返回 ErrNotFound；如果那个引用在**别的命名空间**里存在，
// 返回 *CrossNamespaceError（I11：跨命名空间引用一律 400，不静默当成「不存在」——
// 后者会让「我把 staging 的能力 id 填进 prod 了」变成一个查不到的谜）。
func (d *DB) resolveLinkRef(ctx context.Context, namespaceID, raw, what string) (string, *CapabilityVersion, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil, fmt.Errorf("%w: empty %s", ErrRefUnresolved, what)
	}
	if c, err := d.ResolveRef(ctx, namespaceID, s); err == nil {
		// 原样保存用户给的引用：`name` 就是跟随，`name@1` 就是钉死。
		return s, c, nil
	} else if !errors.Is(err, ErrNotFound) {
		return "", nil, err
	}

	// 兼容：传的是能力 id（P2.2 之前的调用方）。转成 name@version，
	// **不**把 id 写进 ref 列——ref 列只放引用，这是这次迁移的全部意义。
	if c, err := d.GetCapabilityByID(ctx, namespaceID, s); err == nil {
		return FormatRef(c.Name, c.Version), c, nil
	} else if !errors.Is(err, ErrNotFound) {
		return "", nil, err
	}

	// 本命名空间里没有：如果别处有，说明是跨命名空间引用（I11），要明确报出来。
	if other, err := d.namespaceOfCapabilityKey(ctx, s); err != nil {
		return "", nil, err
	} else if other != "" && other != namespaceID {
		return "", nil, &CrossNamespaceError{What: what, Want: namespaceID, Got: other}
	}
	return "", nil, fmt.Errorf("%w: %s %q", ErrNotFound, what, s)
}

// namespaceOfCapabilityKey 在**全局**范围里找一个能力（按引用名或 id），
// 返回它所属的 namespace_id；找不到返回 ""。
//
// 只在解析失败的路径上调用（成功路径一次都不查）：它存在的唯一目的是把
// 「跨命名空间引用」与「写错名字」区分开——两者在只查本命名空间的视图里长得一样，
// 但一个是配置越界（要让用户看见），一个是拼写错误（也要让用户看见，但文案不同）。
func (d *DB) namespaceOfCapabilityKey(ctx context.Context, key string) (string, error) {
	name, version, pinned := SplitRef(key)
	var ns string
	var err error
	if pinned {
		err = d.QueryRowContext(ctx,
			`SELECT namespace_id FROM capability_versions WHERE name = ? AND version = ? LIMIT 1`,
			name, version).Scan(&ns)
	} else {
		err = d.QueryRowContext(ctx,
			`SELECT namespace_id FROM capability_versions WHERE name = ? LIMIT 1`, name).Scan(&ns)
	}
	if err == nil {
		return ns, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("lookup capability namespace by ref: %w", err)
	}
	// 再按 id 试一次（老客户端的形状）
	err = d.QueryRowContext(ctx,
		`SELECT namespace_id FROM capability_versions WHERE id = ?`, key).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup capability namespace by id: %w", err)
	}
	return ns, nil
}

// refLookupKeys 返回「用来匹配这一端引用」的候选值（精确的在前）。
//
// 为什么可能有两个：调用方（控制台 / e2e / 老客户端）可能用**能力 id** 来定位
// 一条 link（P2.2 之前 id 就是主键的一部分）。这时它真正的意图是「这条 link」，
// 而库里那一端可能是 `name@1`，也可能是 `name`（跟随 latest）——两种都要删/查到。
//
// 传进来的本身就是引用（`name` / `name@version`）时**只有一个候选**：
// `fixer@1` 与 `fixer` 是两条不同的 link，绝不能互相匹配。
func (d *DB) refLookupKeys(ctx context.Context, namespaceID, raw string) []string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	if _, _, pinned := SplitRef(s); pinned {
		return []string{s}
	}
	// 先看它是不是一个已知的能力名（是 → 库里就是这个名字，唯一候选）
	if _, err := d.GetCapabilityLatest(ctx, namespaceID, s); err == nil {
		return []string{s}
	}
	// 不是名字：试 id（输入兼容）
	if c, err := d.GetCapabilityByID(ctx, namespaceID, s); err == nil {
		return []string{FormatRef(c.Name, c.Version), c.Name}
	}
	return []string{s}
}

// nsRefIndex 是「一个命名空间的能力定义」在内存里的引用索引（一次查表，多次解析）。
//
// 只在**配置/读**路径上用（源的出口列表、控制台视图）：那里要一次回答「这些引用
// 各指向哪一版」，逐条 ResolveRef 会变成 N 次往返。派发热路径不用它——那里只解析
// 本次派发真正要走的十几条 link，一条 `ResolveRef` 就是「本次派发时刻的 latest」。
type nsRefIndex struct {
	byID     map[string]*CapabilityVersion
	byPinned map[string]*CapabilityVersion
	byName   map[string]*CapabilityVersion
}

// namespaceRefIndex 读出整个命名空间的能力定义并建索引。
func (d *DB) namespaceRefIndex(ctx context.Context, namespaceID string) (*nsRefIndex, error) {
	caps, err := d.ListCapabilities(ctx, namespaceID)
	if err != nil {
		return nil, err
	}
	idx := &nsRefIndex{
		byID:     make(map[string]*CapabilityVersion, len(caps)),
		byPinned: make(map[string]*CapabilityVersion, len(caps)),
		byName:   make(map[string]*CapabilityVersion, len(caps)),
	}
	for _, c := range caps {
		idx.byID[c.ID] = c
		idx.byPinned[FormatRef(c.Name, c.Version)] = c
		// ListCapabilities 按 (name, version) 升序 → 最后写入的就是 latest
		idx.byName[c.Name] = c
	}
	return idx, nil
}

// resolve 按索引解析引用；未命中返回 nil。
func (i *nsRefIndex) resolve(ref string) *CapabilityVersion {
	if i == nil {
		return nil
	}
	name, version, pinned := SplitRef(ref)
	if !pinned {
		return i.byName[name]
	}
	return i.byPinned[FormatRef(name, version)]
}

// idOf 返回引用对应的能力定义 id；解析不到返回空串
// （调用方按“缺定义”处理：引用指向的能力已被删，这一行不该再被当成可用的出口）。
func (i *nsRefIndex) idOf(ref string) string {
	if c := i.resolve(ref); c != nil {
		return c.ID
	}
	return ""
}
