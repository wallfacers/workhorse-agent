# Grep scaling benchmark — 2026-05-27

记录 `BenchmarkGrep_Scaling` 在 i5-14600KF (20 logical cores) / Linux 6.6
WSL2 / NVMe 上的一次实测，用于解释 `tools.grep.workers` 默认值为何被
夹到 `min(runtime.NumCPU(), 8)`。

## 命令

```bash
go test -run='^$' -bench='^BenchmarkGrep_Scaling$' -benchtime=2s -benchmem \
  ./internal/tools/builtin/
```

fixture: 每文件 50 字节 (`// TODO ...`)，按 20 文件/目录铺开，
`.gitignore` 排除 `*.log` 与 `build/`，`RespectGitignore: true`。

## 结果

| files \ workers | 1       | 2       | 4       | 8           | 20      | best speedup |
|-----------------|---------|---------|---------|-------------|---------|--------------|
| 200             | 2.57 ms | 2.48 ms | 2.22 ms | **2.00 ms** | 2.10 ms | 1.29× @ w=8  |
| 2 000           | 4.09 ms | 3.97 ms | 3.03 ms | **2.57 ms** | 2.73 ms | 1.59× @ w=8  |
| 20 000          | 4.65 ms | 4.52 ms | 3.30 ms | **2.91 ms** | 3.09 ms | 1.60× @ w=8  |

## 三个结论

1. **worker pool 在 8 见顶。** 跨规模 `w=8` 都是最优；`w=20` 略退化。
   单 walker goroutine 在 `filepath.WalkDir` 上是瓶颈，向 channel 投递文件
   的速度限制了 worker 实际能消化的吞吐。再开 worker 只增调度抖动。
   → 默认值从 `runtime.NumCPU()` 改为 `min(NumCPU, 8)`
   (`internal/tools/builtin/grep.go:106`)。
2. **并行收益随规模升高。** 200 文件 1.29×、2k+ 文件稳定 1.6×。小仓库
   被 walker startup + 输出排序常数项主导，再多 worker 也没活干。
3. **20k vs 2k 只慢 ~15% — 这是 hot-cache 上限。** fixture 总字节 ~1 MB
   全在 page cache，每文件 scan 摊到 ~150 ns，主要是 syscall 与
   `bufio.Scanner` 常数。提交 `e7dd292` 描述的 "1GB / 30k 文件 → 3 s"
   是 cold-cache + 真实文件大小的代表数；这张表不能用来推断那种场景。

## 真实负载怎么测

```bash
# 在大 monorepo (>RAM) 里冷盘测端到端，需 root
sync && echo 3 | sudo tee /proc/sys/vm/drop_caches

# 起一次性 main 直接调 builtin.Grep{}.Run，或起 serve 后用 MCP 客户端
# 调 grep 工具
go run ./cmd/workhorse-agent serve
```

`BenchmarkGrep_Scaling` 适合回归 (`-count=5` 后用 `benchstat` 比较)，
不适合直接当性能宣称的数字。
