package workspace

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// BenchmarkContentScan uses fixed, explicitly counted inputs. Setup and one
// warming traversal are outside the measurement; p50/p95 summarize the actual
// measured complete traversals, not calibration runs or backend startup. Run:
// go test ./internal/workspace -run '^$' -bench BenchmarkContentScan -benchtime=5x -benchmem
func BenchmarkContentScan(b *testing.B) {
	for _, count := range []int{1000, 10000, 50000} {
		b.Run(fmt.Sprintf("files_%d", count), func(b *testing.B) {
			root := b.TempDir()
			const size = 128
			content := []byte(strings.Repeat("abcdefgh", size/8))
			for i := 0; i < count; i++ {
				dir := filepath.Join(root, fmt.Sprintf("shard_%03d", i/1000))
				if i%1000 == 0 {
					if err := os.Mkdir(dir, 0700); err != nil {
						b.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("input_%06d.bin", i)), content, 0600); err != nil {
					b.Fatal(err)
				}
			}
			benchmarkScans(b, root, count, int64(count*size))
		})
	}
	b.Run("large_64MiB", func(b *testing.B) {
		root := b.TempDir()
		file, err := os.Create(filepath.Join(root, "large.bin"))
		if err != nil {
			b.Fatal(err)
		}
		block := strings.Repeat("abcdefgh", 128*1024)
		for i := 0; i < 64; i++ {
			if _, err := io.WriteString(file, block); err != nil {
				b.Fatal(err)
			}
		}
		if err := file.Close(); err != nil {
			b.Fatal(err)
		}
		benchmarkScans(b, root, 1, 64*1024*1024)
	})
}

func benchmarkScans(b *testing.B, root string, files int, bytes int64) {
	tr, err := New(Config{Workspace: root}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		b.Fatal(err)
	}
	if _, err := tr.Scan(context.Background()); err != nil {
		b.Fatal(err)
	}
	latencies := make([]time.Duration, 0, b.N)
	b.SetBytes(bytes)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		if _, err := tr.Scan(context.Background()); err != nil {
			b.Fatal(err)
		}
		latencies = append(latencies, time.Since(start))
	}
	b.StopTimer()
	slices.Sort(latencies)
	if len(latencies) > 0 {
		b.ReportMetric(float64(latencies[(len(latencies)-1)/2].Nanoseconds())/1e6, "p50-ms")
		b.ReportMetric(float64(latencies[(95*len(latencies)+99)/100-1].Nanoseconds())/1e6, "p95-ms")
	}
	b.ReportMetric(float64(files), "files/scan")
	b.ReportMetric(float64(bytes), "input-bytes/scan")
}
