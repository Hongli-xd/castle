// PoC-1: P0 — Store.Put*() 的 WAL 写入在锁外，与 Compact() 并发时
// 已确认写入的记录会永久丢失。
//
// 复现原理（对应 internal/store/store.go:276-290 vs internal/store/wal.go:233-264）：
//
//	PutSandbox/PutTenant/... 的顺序是：
//	    1) s.append(...)      ← 写 WAL 帧 + fsync（此时【未持锁】）
//	    2) s.mu.Lock(); map[id] = x; Unlock()
//
//	Compact() 的顺序是（全程持写锁）：
//	    1) marshalAll()          ← 从内存 map 取快照
//	    2) rename(tmp, data.json)
//	    3) Truncate(wal.log, 0)  ← 把整个 WAL 清空
//
//	交错：调用方 append 完成（WAL 里已有该帧）→ Compact 抢到锁，此时该记录
//	尚未进内存 map，因此既没写进 data.json，其 WAL 帧又被截断清除 →
//	调用方拿到成功返回，但记录在磁盘上不存在，重启即丢。
//
// 本 PoC 让「一次 Compact」与「若干次 PutTenant」并发，然后关闭并重开 Store，
// 检查 PutTenant 曾返回 nil 的记录是否还在。因为只有一次 Compact、
// 且它是最后一次落盘动作，所以不可能被后续压实"治愈"。
package main

import (
	"fmt"
	"os"
	"sync"

	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/store"
)

const (
	attemptsMax = 120 // 尝试轮次上限
	putsPerTry  = 24  // 每轮并发的 Put 次数
)

func main() {
	lossyRounds := 0
	totalLost := 0
	firstLossRound := -1

	for round := 1; round <= attemptsMax; round++ {
		dir, err := os.MkdirTemp("", "walrace-")
		if err != nil {
			panic(err)
		}

		st, err := store.Open(dir, store.Options{})
		if err != nil {
			panic(err)
		}

		// 一轮 Compact 与若干 Put 并发 —— 与真实 daemon 的
		// 「10 分钟压实循环 + 请求线程写入」是同一个交错。
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = st.Compact()
		}()

		ids := make([]string, 0, putsPerTry)
		for i := 0; i < putsPerTry; i++ {
			id := fmt.Sprintf("tnt_poc_%02d", i)
			if err := st.PutTenant(&domain.Tenant{ID: id, Name: "poc"}); err != nil {
				panic(err)
			}
			ids = append(ids, id) // PutTenant 返回 nil == 已提交成功
		}
		wg.Wait()

		if err := st.Close(); err != nil {
			panic(err)
		}

		// 模拟进程重启：重新打开同一个数据目录
		st2, err := store.Open(dir, store.Options{})
		if err != nil {
			panic(err)
		}
		lost := 0
		for _, id := range ids {
			if _, ok := st2.GetTenant(id); !ok {
				lost++
			}
		}
		_ = st2.Close()
		_ = os.RemoveAll(dir)

		if lost > 0 {
			lossyRounds++
			totalLost += lost
			if firstLossRound < 0 {
				firstLossRound = round
				fmt.Printf("!! 第 %d 轮即复现丢失：%d/%d 条已确认写入的记录在重启后消失\n",
					round, lost, putsPerTry)
			}
		}
	}

	total := attemptsMax * putsPerTry
	fmt.Println("─────────────────────────────────────────────")
	fmt.Printf("总写入（每次均返回成功）: %d 条\n", total)
	fmt.Printf("重启后丢失              : %d 条\n", totalLost)
	fmt.Printf("丢失率                  : %.1f%%\n", float64(totalLost)/float64(total)*100)
	fmt.Printf("复现轮次                : %d/%d 轮出现丢失（首次于第 %d 轮）\n",
		lossyRounds, attemptsMax, firstLossRound)
	if totalLost > 0 {
		fmt.Println("结论: [已复现] PutTenant 返回成功 ≠ 记录已持久化")
		os.Exit(1)
	}
	fmt.Println("结论: 本轮未复现（窗口未命中，可重跑）")
}
