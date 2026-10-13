// PoC-3: P1 — 单写者目录锁用的是【包级全局变量】，因此：
//
//	(a) 打开第二个 Store 会覆盖 lockFile，第一个 Store 的锁 fd 永久泄漏；
//	(b) 关闭第一个 Store 会解锁【第二个】数据目录的锁 —— 而第二个 Store 还开着。
//
// 缺陷位置：internal/store/wal.go:285-308
//
//	var lockFile *os.File                 // ← 包级全局，不是 Store 的字段
//	func lockDir(dir string) error { ...; lockFile = f; return nil }
//	func unlockDir() error { ...; syscall.Flock(int(lockFile.Fd()), LOCK_UN); lockFile.Close(); lockFile = nil }
//
// 影响：THREAT-MODEL.md 与 DEPLOYMENT.md 都声称"flock 防双进程共写"。但
// 只要同一进程内出现过第二个 Store（例如备份/校验工具、或测试里打开另一个
// 数据目录），这个保证就失效 —— 第二个数据目录会在仍被使用时失去互斥锁，
// 另一个进程可以随即取得写权限，造成双写者交错破坏 WAL。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// import 了 store 才会加载其 init/全局变量
import "sandcastle.dev/sandcastle/internal/store"

func fdCount() int {
	ents, err := os.ReadDir("/dev/fd")
	if err != nil {
		return -1
	}
	return len(ents)
}

// canLockExclusively 尝试以独立 open() 取得该文件的排他 flock。
// 同一进程内由不同 open() 得到的 fd 会互相冲突（POSIX/Linux flock 语义），
// 所以这等价于"另一个进程能否拿走这把锁"。
func canLockExclusively(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return true
	}
	return false
}

func main() {
	base, err := os.MkdirTemp("", "lockleak-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(base)
	dirA := filepath.Join(base, "A")
	dirB := filepath.Join(base, "B")

	fd0 := fdCount()

	stA, err := store.Open(dirA, store.Options{})
	if err != nil {
		panic(err)
	}
	fd1 := fdCount()

	stB, err := store.Open(dirB, store.Options{})
	if err != nil {
		panic(err)
	}
	fd2 := fdCount()

	lockA := filepath.Join(dirA, "lock")
	lockB := filepath.Join(dirB, "lock")

	fmt.Println("── 步骤 1：两个 Store 同时打开（各自数据目录）")
	fmt.Printf("   open 的 fd 数: 起始 %d → 开 A 后 %d → 开 B 后 %d\n", fd0, fd1, fd2)
	// dirA 与 dirB 都各有一个 lock 文件被 flock 持有
	lockedA := !canLockExclusively(lockA)
	lockedB := !canLockExclusively(lockB)
	fmt.Printf("   A 的锁: %s    B 的锁: %s\n\n",
		map[bool]string{true: "持有中 ✓", false: "【未持有】✗"}[lockedA],
		map[bool]string{true: "持有中 ✓", false: "【未持有】✗"}[lockedB])

	fmt.Println("── 步骤 2：关闭 A（B 仍然开着）")
	_ = stA.Close()
	// true == 能拿到排他锁 == 该锁已被释放
	aFreed := canLockExclusively(lockA)
	bFreed := canLockExclusively(lockB)

	fmt.Printf("   A 的锁: %s\n", map[bool]string{true: "已释放 ✓", false: "【仍被持有】✗"}[aFreed])
	fmt.Printf("   B 的锁: %s\n\n",
		map[bool]string{true: "意外地【被释放了】✗", false: "仍被持有 ✓"}[bFreed])

	_ = stB.Close()
	fdEnd := fdCount()

	fmt.Println("─────────────────────────────────────────────")
	fail := false
	if bFreed {
		fmt.Println("结论(b): [已复现] 关闭 A 时把【还在使用的 B 目录】的排他锁也解掉了")
		fmt.Println("         → 此时另一个进程可以取得 B 的写锁，与仍存活的 stB 构成双写者")
		fail = true
	}
	if fd2-fd1 > 1 {
		fmt.Printf("结论(a): [已复现] 开 A 后 fd 增量 %d（A 需要 2 个: wal + lock）\n", fd1-fd0)
	}
	if fdEnd > fd0 {
		fmt.Printf("         [已复现] 全部关闭后仍残留 %d 个 fd（A 的 lock 从未被 Close）\n", fdEnd-fd0)
		fmt.Println("         → 这是 fd 泄漏；且因锁 fd 未释放，A 的目录锁只能等进程退出才消失")
		fail = true
	}
	if !fail {
		fmt.Println("结论: 未复现")
	}
	if fail {
		os.Exit(1)
	}
}
