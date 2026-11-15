package pandu

import (
	"fmt"
	"io"
	"time"
)

func uploadReporter(out io.Writer, target string) func(done, total int) {
	if out == nil {
		return nil
	}
	fmt.Fprintf(out, "正在请求网盘预上传 %q…\n", target)
	var lastUpdate time.Time
	return func(done, total int) {
		if total == 0 {
			fmt.Fprintf(out, "网盘已有全部分片，正在合并 %q…\n", target)
			return
		}
		now := time.Now()
		if done == 0 || done == total || now.Sub(lastUpdate) >= 2*time.Second {
			fmt.Fprintf(out, "上传 %q · %d/%d 个待传分片（%.1f%%）\n", target, done, total, float64(done)*100/float64(total))
			lastUpdate = now
		}
		if done == total {
			fmt.Fprintf(out, "正在合并 %q…\n", target)
		}
	}
}
