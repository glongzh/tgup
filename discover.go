package main

// 文件发现与 --plan 预估。对应 tgup.py 的 discover / print_plan。

import (
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// fileMeta 是筛选与排序都要用的元信息。扫描时顺手记下来，
// 避免后面为了排序、为了过滤大小再反复 Stat 同一批文件。
type fileMeta struct {
	size  int64
	mtime int64
}

// discover 递归/单层收集视频文件，按大小/时间/名称排序，去重。
// 注意：直接指定的文件不受扩展名白名单约束（与 Python 版一致），仅目录扫描时过滤。
func discover(inputs []string, recursive bool, exts map[string]struct{},
	minSize, maxSize int64, sortBy string, reverse bool) []string {

	found := map[string]fileMeta{}
	explicit := map[string]struct{}{}

	addFile := func(p string, explicitFile bool) {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		} else if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		// 显式点名优先级更高：同一个文件既被直接指定、又在目录里被扫到时，
		// 它应当享有「不受扩展名白名单约束」的待遇。所以先记 explicit 再去重。
		if explicitFile {
			explicit[p] = struct{}{}
		}
		if _, dup := found[p]; dup {
			return
		}
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			return
		}
		found[p] = fileMeta{size: fi.Size(), mtime: fi.ModTime().UnixNano()}
	}

	wanted := func(name string) bool {
		_, ok := exts[strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")]
		return ok
	}

	for _, item := range inputs {
		p := expandTilde(item)
		fi, err := os.Stat(p)
		if err != nil {
			logf("⚠ 跳过不存在的路径: %s", item)
			continue
		}
		switch {
		case fi.Mode().IsRegular():
			addFile(p, true)

		case fi.IsDir() && recursive:
			_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil // 无权限之类，跳过
				}
				if d.IsDir() {
					if path == p {
						return nil // 用户点名的根目录本身永远不跳过
					}
					// 隐藏目录（.git / .cache / .Trash）和上次中断留下的分片目录整体跳过。
					// 这里必须返回 SkipDir：返回 nil 只跳过该条目本身，
					// WalkDir 仍然会往下递归，等于没跳过。
					if strings.HasPrefix(d.Name(), ".") ||
						strings.HasPrefix(d.Name(), SPLIT_DIR_PREFIX) {
						return filepath.SkipDir
					}
					return nil
				}
				if strings.HasPrefix(d.Name(), ".") {
					return nil
				}
				if wanted(d.Name()) {
					addFile(path, false)
				}
				return nil
			})

		case fi.IsDir():
			entries, err := os.ReadDir(p)
			if err != nil {
				logf("⚠ 跳过不可读目录: %s", item)
				continue
			}
			for _, e := range entries {
				if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				if !e.Type().IsRegular() {
					continue
				}
				if wanted(e.Name()) {
					addFile(filepath.Join(p, e.Name()), false)
				}
			}
		}
	}

	out := make([]string, 0, len(found))
	for f, m := range found {
		if m.size < minSize || (maxSize > 0 && m.size > maxSize) {
			continue
		}
		if _, isExplicit := explicit[f]; !isExplicit && !wanted(f) {
			continue
		}
		out = append(out, f)
	}

	// 先按路径排一遍，让后面的 SliceStable 有确定的输入顺序。
	// out 是从 map 迭代出来的，顺序本身就是随机的；如果直接排序，
	// 所有「比较键相等」的文件（比如一批同为 1GB 的视频）相对顺序会每次运行都不同，
	// 上传顺序也就不可复现。先定序再稳定排序，等值项自然落到字典序。
	sort.Strings(out)

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		var less, equal bool
		switch sortBy {
		case "name":
			la, lb := strings.ToLower(a), strings.ToLower(b)
			less, equal = la < lb, la == lb
		case "size":
			less, equal = found[a].size < found[b].size, found[a].size == found[b].size
		default: // mtime
			less, equal = found[a].mtime < found[b].mtime, found[a].mtime == found[b].mtime
		}
		// 等值时一律返回 false。比较函数必须构成严格弱序，
		// 否则 sort 会给出不确定的顺序——旧实现写成 `return !less`，
		// 两个文件等值时返回 true，既违反自反性又让 --reverse 的结果随机。
		if equal {
			return false
		}
		if reverse {
			return !less
		}
		return less
	})
	return out
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// printPlan 只打印计划与耗时预估，不真传。
func printPlan(files []string, rate float64, dailyCap int64,
	burstS, restS float64, windows []TimeWindow) {
	var total int64
	for _, f := range files {
		total += fileSize(f)
	}
	logf("待传 %d 个文件，合计 %s", len(files), human(float64(total)))
	for i, f := range files {
		if i == 15 {
			logf("    … 另有 %d 个", len(files)-15)
			break
		}
		logf("    %9s  %s", human(float64(fileSize(f))), f)
	}

	if rate <= 0 {
		log("\n未设置 --rate，将全速上传（不推荐用于大批量）")
		return
	}

	eff := rate
	if burstS > 0 && restS > 0 {
		eff *= burstS / (burstS + restS)
	}
	dailySeconds := 24 * 3600
	if len(windows) > 0 {
		dailySeconds = 0
		for _, w := range windows {
			span := w.End - w.Start // 分钟
			if span <= 0 {
				span += 24 * 60
			}
			dailySeconds += span * 60
		}
	}
	perDay := eff * float64(dailySeconds)
	if dailyCap > 0 && perDay > float64(dailyCap) {
		perDay = float64(dailyCap)
	}

	logf("\n有效速率 %s/s（占空比与限速折算后）", human(eff))
	logf("每日可传 ≈ %s", human(perDay))
	if perDay > 0 {
		logf("预计耗时 ≈ %.1f 天", float64(total)/perDay)
	} else {
		log("预计耗时：无法估算")
	}
}

// jitterValue 给数值加 ±ratio 的随机抖动（对应 Pacer._jit）。
func jitterValue(v, ratio float64) float64 {
	return v * (1 - ratio + 2*ratio*rand.Float64())
}
