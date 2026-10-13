// PoC-2: P0 — 撕裂写之后的"回滚"曾经是空操作，导致一次 I/O 错误之后
// 【所有已 fsync 成功的事务】在重启时被静默丢弃。
//
// ── 缺陷（v1.1.0）───────────────────────────────────────────────
//
// 回滚代码（wal.go:43-53）：
//
//	n, err := s.wal.Write(buf)
//	if err != nil {
//	    _ = s.wal.Truncate(int64(s.walSize()))   // ← walSize() 返回【写入之后】的
//	    return ...                                //    文件大小，截断到当前大小 = 空操作
//	}
//
// 且恢复路径 replayAndRepair 在首个坏帧处 break，把该点起【全部】截断，
// 没有任何日志、告警或指标 → 残帧之后每一个"已提交"事务都被静默丢弃。
//
// ── 修复（v1.1.1）──────────────────────────────────────────────
//
//  1. appendLocked 在写入【之前】记录偏移，失败时回滚到该偏移（fsync 失败也回滚，
//     否则会写出一个"被告知失败却可重放"的幽灵事务）；
//  2. 恢复路径先【分类】再处置：尾部撕裂是常规崩溃残骸（可丢），
//     日志中段损坏是已确认数据（默认拒绝启动，必须人工介入）；
//  3. 引入 RepairTailOnly（默认）/ RepairSalvage / RepairStrict 三种策略，
//     并对每次恢复产出 RepairReport。
//
// 本 PoC 现在是一个验证 harness：构造各种日志布局，断言新策略的行为。
// 若默认策略下出现"中段损坏被静默截断"，本程序以非 0 退出。
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"

	"sandcastle.dev/sandcastle/internal/store"
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// frame 与 internal/store/store.go 的未导出结构逐字段对齐
type frame struct {
	K    string          `json:"k"`
	Op   string          `json:"op"`
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data,omitempty"`
}

// makeFrame 按 [len u32 BE][crc32 u32 BE][payload] 组帧
func makeFrame(k, op, id string, data any) ([]byte, error) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	payload, err := json.Marshal(frame{K: k, Op: op, ID: id, Data: raw})
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(payload)))
	binary.BigEndian.PutUint32(buf[4:8], crc32.Checksum(payload, crcTable))
	copy(buf[8:], payload)
	return buf, nil
}

func tenantFrame(id string) []byte {
	b, err := makeFrame("tenant", "put", id, map[string]string{"id": id, "name": id})
	if err != nil {
		panic(err)
	}
	return b
}

// corruptCRC 把一帧的 CRC 改坏，模拟位翻转；长度头保持合法，
// 所以"下一帧边界"是可推导的——这正是真实的中段损坏形态。
func corruptCRC(f []byte) []byte {
	c := append([]byte(nil), f...)
	binary.BigEndian.PutUint32(c[4:8], 0xDEADBEEF)
	return c
}

var failures int

func check(ok bool, format string, args ...any) {
	mark := "✓"
	if !ok {
		mark = "✗"
		failures++
	}
	fmt.Printf("  %s %s\n", mark, fmt.Sprintf(format, args...))
}

// openWith 写入构造好的日志并尝试打开 store。
func openWith(blob []byte, policy store.RepairPolicy) (st *store.Store, err error, dir string) {
	dir, err = os.MkdirTemp("", "tornframe-")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wal.log"), blob, 0o600); err != nil {
		panic(err)
	}
	st, err = store.Open(dir, store.Options{Repair: policy})
	return st, err, dir
}

func main() {
	fmt.Println("═══ 场景 A：尾部撕裂（掉电/被杀的正常崩溃残骸）")
	// [已提交1][已提交2][写了一半的第3帧]
	half := tenantFrame("tnt_HALF")
	blobTail := concat(tenantFrame("tnt_GOOD_1"), tenantFrame("tnt_GOOD_2"), half[:len(half)/2])
	st, err, dir := openWith(blobTail, store.RepairTailOnly)
	switch {
	case err != nil:
		check(false, "默认策略应能自动修复尾部撕裂，却拒绝启动: %v", err)
	default:
		rep := st.Repair()
		check(rep.Damaged && rep.Kind == "torn-tail", "识别为尾部撕裂 (kind=%s)", rep.Kind)
		check(rep.Action == "truncate", "自动截断修复 (action=%s)", rep.Action)
		check(has(st, "tnt_GOOD_1") && has(st, "tnt_GOOD_2"), "撕裂点之前的 2 条已提交事务保住了")
		check(!has(st, "tnt_HALF"), "写了一半的那条没有进来（它本就不是已提交事务）")
		fmt.Printf("    报告: %s\n", rep)
		st.Close()
	}
	os.RemoveAll(dir)

	fmt.Println("\n═══ 场景 B：日志【中段】损坏，其后还有 fsync 成功的已提交事务")
	// [已提交1][坏帧][已提交3][已提交4] —— v1.1.0 在这里静默丢弃了 3 和 4
	blobMid := concat(tenantFrame("tnt_BEFORE"), corruptCRC(tenantFrame("tnt_TORN")),
		tenantFrame("tnt_AFTER_1"), tenantFrame("tnt_AFTER_2"))
	st, err, dir = openWith(blobMid, store.RepairTailOnly)
	check(err != nil, "默认策略拒绝启动（不再静默丢弃已确认数据）")
	if err != nil {
		msg := err.Error()
		check(strings.Contains(msg, "mid-log"), "错误信息点明是日志中段损坏")
		check(strings.Contains(msg, "salvage"), "错误信息给出逃生舱（repair=salvage）")
		fmt.Printf("    %s\n", msg)
	} else {
		st.Close()
	}
	os.RemoveAll(dir)

	fmt.Println("\n═══ 场景 C：同一布局 + 显式 RepairSalvage（运维主动接受尾部丢失）")
	st, err, dir = openWith(blobMid, store.RepairSalvage)
	if err != nil {
		check(false, "salvage 策略应当允许启动，却失败: %v", err)
	} else {
		rep := st.Repair()
		check(rep.Kind == "mid-log" && rep.Action == "salvage",
			"报告如实记录：中段损坏 + 人工 salvage (kind=%s action=%s)", rep.Kind, rep.Action)
		check(rep.BytesDropped > 0, "丢弃了多少字节被明确量化 (%d B)", rep.BytesDropped)
		check(has(st, "tnt_BEFORE"), "损坏点之前的已提交事务保留")
		check(!has(st, "tnt_AFTER_1") && !has(st, "tnt_AFTER_2"),
			"损坏点之后的已提交事务确实被丢弃——但这是显式选择的，且有报告")
		fmt.Printf("    报告: %s\n", rep)
		st.Close()
	}
	os.RemoveAll(dir)

	fmt.Println("\n═══ 场景 D：RepairStrict —— 连尾部撕裂也不自动修，一律要人来看")
	st, err, dir = openWith(blobTail, store.RepairStrict)
	check(err != nil, "strict 策略拒绝启动")
	if err == nil {
		st.Close()
	}
	os.RemoveAll(dir)

	fmt.Println("\n═══ 场景 E：完好日志不应被误判为损坏")
	blobOK := concat(tenantFrame("tnt_OK_1"), tenantFrame("tnt_OK_2"))
	st, err, dir = openWith(blobOK, store.RepairTailOnly)
	if err != nil {
		check(false, "完好日志竟然打不开: %v", err)
	} else {
		check(!st.Repair().Damaged, "报告为 intact")
		check(has(st, "tnt_OK_1") && has(st, "tnt_OK_2"), "两条事务都在")
		st.Close()
	}
	os.RemoveAll(dir)

	fmt.Println("\n─────────────────────────────────────────────")
	if failures > 0 {
		fmt.Printf("结论: [未通过] %d 项断言失败 —— 静默丢数据的行为仍在\n", failures)
		os.Exit(1)
	}
	fmt.Println("结论: [已修复] 尾部撕裂自动修复；中段损坏默认拒绝启动、可显式 salvage，且全程有量化报告")
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func has(st *store.Store, id string) bool {
	_, ok := st.GetTenant(id)
	return ok
}
