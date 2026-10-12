package fscat

import (
	"fmt"
	"testing"
)

// BenchmarkScanWorkers measures the full scan for several worker counts (INCUS_BACKUP_BENCH_FILES).
func BenchmarkScanWorkers(b *testing.B) {
	root := benchTree(b, benchFiles())
	for _, w := range []int{1, 2, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("workers=%d", w), func(b *testing.B) {
			b.ReportAllocs()
			var n int
			for i := 0; i < b.N; i++ {
				res, err := Scan(root, ScanOptions{Workers: w})
				if err != nil {
					b.Fatal(err)
				}
				n = res.Tree.Count
			}
			b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds(), "entries/s")
		})
	}
}
