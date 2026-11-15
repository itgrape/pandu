package pandu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/baidu-netdisk/baidu-drive-sdk-go/baidudriver/api"
)

type SyncOptions struct {
	DryRun         bool
	Delete         bool
	UploadProgress io.Writer
}

type syncRecord struct {
	Remote remoteVersion `json:"remote"`
	SHA256 string        `json:"sha256"`
}

type syncIndex struct {
	SessionID string                `json:"session_id"`
	Files     map[string]syncRecord `json:"files"`
}

func (s *session) readIndex() (syncIndex, error) {
	var index syncIndex
	err := readJSON(s.path+".sync.json", &index)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return syncIndex{}, fmt.Errorf("读取增量状态: %w", err)
	}
	if index.SessionID != s.token.SessionID || index.Files == nil {
		index = syncIndex{s.token.SessionID, make(map[string]syncRecord)}
	}
	return index, nil
}

type syncItem struct {
	rel     string
	local   fs.FileInfo
	remote  *api.ListFileInfo
	dir     bool
	makeDir bool
	remove  bool
}

func syncFiles(ctx context.Context, out io.Writer, s *session, local *localTree, remotePath string, upload bool, options SyncOptions) (err error) {
	if !upload || options.DryRun {
		options.UploadProgress = nil
	}
	if options.UploadProgress != nil {
		fmt.Fprintf(options.UploadProgress, "正在扫描 %q 并对比网盘目录…\n", local.absolute)
	}
	drive := &remote{session: s}
	items, parents, err := planSync(ctx, local, drive, remotePath, upload, options.Delete)
	if err != nil {
		return err
	}
	index, err := s.readIndex()
	if err != nil {
		return err
	}
	dirty := false
	defer func() {
		if dirty {
			if saveErr := writeJSON(s.path+".sync.json", &index); saveErr != nil {
				err = errors.Join(err, fmt.Errorf("保存增量状态: %w", saveErr))
			}
		}
	}()

	for _, parent := range parents {
		if err := reportAction(out, options.DryRun, "mkdir", parent, func() error {
			return drive.mkdir(ctx, parent)
		}); err != nil {
			return err
		}
	}
	var transferred, skipped, created, deleted int
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		netPath := path.Join(remotePath, item.rel)
		target := netPath
		if !upload {
			target = filepath.Join(local.absolute, filepath.FromSlash(item.rel))
		}
		switch {
		case item.remove:
			err := reportAction(out, options.DryRun, "delete", target, func() error {
				if upload {
					if err := drive.delete(ctx, item.remote); err != nil {
						return err
					}
					if _, ok := index.Files[netPath]; ok {
						delete(index.Files, netPath)
						dirty = true
					}
					return nil
				}
				return local.delete(item.rel, item.local)
			})
			if err != nil {
				return err
			}
			deleted++
		case item.makeDir:
			if err := reportAction(out, options.DryRun, "mkdir", target, func() error {
				if upload {
					return drive.mkdir(ctx, netPath)
				}
				return local.mkdir(item.rel)
			}); err != nil {
				return err
			}
			created++
		case !item.dir:
			record, changed, err := syncFile(ctx, out, local, drive, netPath, item, index.Files[netPath], upload, options)
			if err != nil {
				return fmt.Errorf("同步 %q: %w", item.rel, err)
			}
			if !changed {
				skipped++
			} else {
				transferred++
				if !options.DryRun {
					index.Files[netPath] = record
					dirty = true
				}
			}
		}
	}
	label := "完成"
	if options.DryRun {
		label = "预览"
	}
	_, err = fmt.Fprintf(out, "%s · 传输 %d，跳过 %d，创建目录 %d，删除 %d\n", label, transferred, skipped, created+len(parents), deleted)
	return err
}

func planSync(ctx context.Context, local *localTree, drive *remote, remotePath string, upload, remove bool) ([]syncItem, []string, error) {
	localTop, err := local.stat(".")
	if err != nil {
		return nil, nil, err
	}
	remoteTop, err := drive.stat(ctx, remotePath)
	if err != nil {
		return nil, nil, err
	}
	if upload && localTop == nil {
		return nil, nil, fmt.Errorf("本地源路径不存在: %q", local.absolute)
	}
	if !upload && remoteTop == nil {
		return nil, nil, fmt.Errorf("网盘源路径不存在: %q", remotePath)
	}
	sourceDir := upload && localTop.IsDir() || !upload && remoteTop.Isdir == 1
	if remove && (!sourceDir || (upload && remotePath == "/") || (!upload && local.base == ".")) {
		return nil, nil, errors.New("--delete 只用于目录同步，且目标不能是根目录")
	}

	localFiles := make(map[string]fs.FileInfo)
	remoteFiles := make(map[string]*api.ListFileInfo)
	scanLocal := func(descend func(string) bool) error {
		var err error
		localFiles, err = local.scan(ctx, localTop, descend)
		return err
	}
	scanRemote := func(descend func(string) bool) error {
		if remoteTop == nil {
			return nil
		}
		remoteFiles["."] = remoteTop
		if remoteTop.Isdir != 1 || !sourceDir {
			return nil
		}
		prefix := strings.TrimSuffix(remotePath, "/") + "/"
		return drive.walk(ctx, remotePath, func(f *api.ListFileInfo) (bool, error) {
			rel := strings.TrimPrefix(f.Path, prefix)
			remoteFiles[rel] = f
			return descend(rel), nil
		})
	}
	if upload {
		if err := scanLocal(func(string) bool { return true }); err != nil {
			return nil, nil, err
		}
		if err := scanRemote(func(rel string) bool {
			item := localFiles[rel]
			return remove || item != nil && item.IsDir()
		}); err != nil {
			return nil, nil, err
		}
	} else {
		if err := scanRemote(func(string) bool { return true }); err != nil {
			return nil, nil, err
		}
		if sourceDir {
			err = scanLocal(func(rel string) bool {
				item := remoteFiles[rel]
				return remove || item != nil && item.Isdir == 1
			})
		} else if localTop != nil {
			localFiles["."] = localTop
		}
		if err != nil {
			return nil, nil, err
		}
	}

	var names, extra []string
	for rel := range localFiles {
		if upload {
			names = append(names, rel)
		} else if remove && remoteFiles[rel] == nil {
			extra = append(extra, rel)
		}
	}
	for rel := range remoteFiles {
		if !upload {
			names = append(names, rel)
		} else if remove && localFiles[rel] == nil {
			extra = append(extra, rel)
		}
	}
	slices.Sort(names)
	slices.Sort(extra)
	slices.Reverse(extra) // Children must be removed before their parents.
	var items []syncItem
	for _, rel := range names {
		localFile, remoteFile := localFiles[rel], remoteFiles[rel]
		if localFile != nil && !localFile.IsDir() && !localFile.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("同步路径含符号链接或特殊文件: %q", rel)
		}
		if localFile != nil && remoteFile != nil && localFile.IsDir() != (remoteFile.Isdir == 1) {
			return nil, nil, fmt.Errorf("同名文件与目录冲突: %q", rel)
		}
		if upload {
			netPath := path.Join(remotePath, rel)
			if (rel != "." && strings.ContainsAny(rel, "\\\x00")) || (!localFile.IsDir() && netPath == "/") {
				return nil, nil, fmt.Errorf("无法映射本地路径到网盘: %q", rel)
			}
		} else if _, err := local.name(rel); err != nil {
			return nil, nil, err
		}
		if remoteFile != nil && remoteFile.Isdir != 1 && (remoteFile.Size < 0 || remoteFile.FsID <= 0) {
			return nil, nil, fmt.Errorf("网盘文件元数据不完整: %q", rel)
		}
		dir := (localFile != nil && localFile.IsDir()) || (remoteFile != nil && remoteFile.Isdir == 1)
		items = append(items, syncItem{rel: rel, local: localFile, remote: remoteFile, dir: dir,
			makeDir: dir && (localFile == nil || remoteFile == nil)})
	}
	for _, rel := range extra {
		items = append(items, syncItem{rel: rel, local: localFiles[rel], remote: remoteFiles[rel], remove: true})
	}

	var parents []string
	if upload && remoteTop == nil {
		for name := path.Dir(remotePath); name != "/"; name = path.Dir(name) {
			parent, err := drive.stat(ctx, name)
			if err != nil {
				return nil, nil, err
			}
			if parent != nil {
				if parent.Isdir != 1 {
					return nil, nil, fmt.Errorf("网盘父路径不是目录: %q", name)
				}
				break
			}
			parents = append(parents, name)
		}
		slices.Reverse(parents)
	}
	return items, parents, nil
}

func syncFile(ctx context.Context, out io.Writer, local *localTree, drive *remote, netPath string, item syncItem, record syncRecord, upload bool, options SyncOptions) (syncRecord, bool, error) {
	var checksum string
	var blocks []string
	var file *os.File
	known := record.SHA256 != "" && item.remote != nil && record.Remote == version(item.remote)
	if item.local != nil && (upload || known && item.remote.Size == item.local.Size()) {
		f, err := local.openFile(item.rel, item.local)
		if err != nil {
			return syncRecord{}, false, err
		}
		if upload {
			file = f
			defer file.Close()
			if options.UploadProgress != nil {
				fmt.Fprintf(options.UploadProgress, "正在计算校验值 %q（%d 字节）…\n", netPath, item.local.Size())
			}
		}
		checksum, blocks, err = hashFile(ctx, f, item.local, upload)
		if !upload {
			err = errors.Join(err, f.Close())
		}
		if err != nil {
			return syncRecord{}, false, err
		}
	}
	if known && record.SHA256 == checksum {
		return syncRecord{}, false, nil
	}

	result := syncRecord{SHA256: checksum}
	if upload {
		err := reportAction(out, options.DryRun, "put", netPath, func() error {
			var err error
			result.Remote, err = drive.upload(ctx, netPath, file, item.local.Size(), blocks, uploadReporter(options.UploadProgress, netPath))
			return err
		})
		return result, true, err
	}
	result.Remote = version(item.remote)
	err := reportAction(out, options.DryRun, "get", filepath.Join(local.absolute, filepath.FromSlash(item.rel)), func() error {
		request, cancel := context.WithTimeout(ctx, 2*time.Hour)
		defer cancel()
		body, err := drive.download(request, item.remote)
		if err != nil {
			return err
		}
		defer body.Close()
		expected := ""
		if known {
			expected = record.SHA256
		}
		mtime := item.remote.LocalMtime
		if mtime <= 0 {
			mtime = item.remote.ServerMtime
		}
		result.SHA256, err = local.receive(request, item.rel, item.local, body, item.remote.Size, expected, mtime)
		return err
	})
	return result, true, err
}

func reportAction(out io.Writer, dryRun bool, action, target string, run func() error) error {
	if !dryRun {
		if err := run(); err != nil {
			return fmt.Errorf("%s %q: %w", action, target, err)
		}
	}
	_, err := fmt.Fprintf(out, "%s\t%q\n", action, target)
	return err
}
