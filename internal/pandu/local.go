package pandu

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

type localTree struct {
	root     *os.Root
	base     string
	absolute string
}

func openLocal(name string) (*localTree, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	anchor, err := existingLocalParent(absolute)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(anchor)
	if err != nil {
		return nil, err
	}
	base, err := filepath.Rel(anchor, absolute)
	if err != nil {
		root.Close()
		return nil, err
	}
	return &localTree{root, base, absolute}, nil
}

func existingLocalParent(absolute string) (string, error) {
	for parent := filepath.Dir(absolute); ; parent = filepath.Dir(parent) {
		info, err := os.Stat(parent)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("本地父路径不是目录: %q", parent)
			}
			return parent, nil
		}
		if !errors.Is(err, fs.ErrNotExist) || parent == filepath.Dir(parent) {
			return "", err
		}
	}
}

func resolveLocal(absolute string) (string, error) {
	parent, err := existingLocalParent(absolute)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(parent, absolute)
	if err != nil {
		return "", err
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, rel), nil
}

func (l *localTree) name(rel string) (string, error) {
	native, err := filepath.Localize(rel)
	if err != nil {
		return "", fmt.Errorf("此系统无法表示文件名 %q: %w", rel, err)
	}
	return filepath.Join(l.base, native), nil
}

func (l *localTree) stat(rel string) (fs.FileInfo, error) {
	name, err := l.name(rel)
	if err != nil {
		return nil, err
	}
	info, err := l.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return info, err
}

func (l *localTree) scan(ctx context.Context, top fs.FileInfo, descend func(string) bool) (map[string]fs.FileInfo, error) {
	files := make(map[string]fs.FileInfo)
	if top == nil {
		return files, nil
	}
	files["."] = top
	if !top.IsDir() {
		return files, nil
	}
	if err := fs.WalkDir(l.root.FS(), filepath.ToSlash(l.base), func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(l.base, filepath.FromSlash(name))
		if err != nil || rel == "." {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		files[rel] = info
		if info.IsDir() && !descend(rel) {
			return fs.SkipDir
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return files, nil
}

func sameLocal(expected, current fs.FileInfo) bool {
	if expected == nil || current == nil {
		return expected == nil && current == nil
	}
	return os.SameFile(expected, current) && expected.Mode() == current.Mode() &&
		(expected.IsDir() || expected.Size() == current.Size() && expected.ModTime().Equal(current.ModTime()))
}

func (l *localTree) openFile(rel string, expected fs.FileInfo) (*os.File, error) {
	name, err := l.name(rel)
	if err != nil {
		return nil, err
	}
	current, err := l.stat(rel)
	if err != nil {
		return nil, err
	}
	if !sameLocal(expected, current) {
		return nil, errors.New("本地文件在同步期间发生变化")
	}
	f, err := l.root.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !sameLocal(expected, info) {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("本地文件在同步期间发生变化")
	}
	return f, nil
}

type contextReader struct {
	context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

// hashFile collects the content checksum and, for uploads, the SDK's ordered
// 4 MiB block checksums in a single pass through the open file.
func hashFile(ctx context.Context, f *os.File, expected fs.FileInfo, forUpload bool) (string, []string, error) {
	digest := sha256.New()
	reader := contextReader{ctx, f}
	var blocks []string
	if forUpload {
		chunk := make([]byte, max(1, min(expected.Size(), sliceSize)))
		for {
			n, err := io.ReadFull(reader, chunk)
			if n > 0 || len(blocks) == 0 && err == io.EOF {
				_, _ = digest.Write(chunk[:n])
				sum := md5.Sum(chunk[:n])
				blocks = append(blocks, hex.EncodeToString(sum[:]))
			}
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			if err != nil {
				return "", nil, err
			}
		}
	} else if _, err := io.Copy(digest, reader); err != nil {
		return "", nil, err
	}
	current, err := f.Stat()
	if err != nil {
		return "", nil, err
	}
	if !sameLocal(expected, current) {
		return "", nil, errors.New("计算校验值期间文件发生变化")
	}
	return hex.EncodeToString(digest.Sum(nil)), blocks, nil
}

func (l *localTree) mkdir(rel string) error {
	name, err := l.name(rel)
	if err != nil {
		return err
	}
	if rel == "." {
		if err := l.root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
	}
	return l.root.Mkdir(name, 0o755)
}

func (l *localTree) delete(rel string, expected fs.FileInfo) error {
	current, err := l.stat(rel)
	if err != nil || current == nil {
		return err
	}
	if !sameLocal(expected, current) {
		return errors.New("本地条目在同步期间发生变化")
	}
	name, err := l.name(rel)
	if err != nil {
		return err
	}
	return l.root.Remove(name)
}

func (l *localTree) receive(ctx context.Context, rel string, expected fs.FileInfo, body io.Reader, size int64, checksum string, mtime int64) (string, error) {
	name, err := l.name(rel)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(name)
	if rel == "." {
		if err := l.root.MkdirAll(parent, 0o755); err != nil {
			return "", err
		}
	}
	temp := filepath.Join(parent, ".pandu-"+rand.Text()+".tmp")
	mode := fs.FileMode(0o600)
	if expected != nil {
		mode = expected.Mode().Perm()
	}
	f, err := l.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return "", err
	}
	defer l.root.Remove(temp)
	defer f.Close()

	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, digest), contextReader{ctx, body})
	if err != nil {
		return "", err
	}
	if written != size {
		return "", fmt.Errorf("下载了 %d 字节，预期 %d 字节", written, size)
	}
	sum := hex.EncodeToString(digest.Sum(nil))
	if checksum != "" && checksum != sum {
		return "", errors.New("下载内容与已知的 SHA-256 校验值不一致")
	}
	if mtime > 0 {
		stamp := time.Unix(mtime, 0)
		if err := l.root.Chtimes(temp, stamp, stamp); err != nil {
			return "", err
		}
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	current, err := l.stat(rel)
	if err != nil {
		return "", err
	}
	if !sameLocal(expected, current) {
		return "", errors.New("本地目标文件在同步期间发生变化")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return sum, l.root.Rename(temp, name)
}
