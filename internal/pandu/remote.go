package pandu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/baidu-netdisk/baidu-drive-sdk-go/baidudriver/api"
)

type remote struct {
	session *session
	client  *api.Client
}

type remoteVersion struct {
	ID   int64  `json:"id"`
	Size int64  `json:"size"`
	MD5  string `json:"md5,omitempty"`
}

func version(file *api.ListFileInfo) remoteVersion {
	return remoteVersion{file.FsID, file.Size, strings.ToLower(file.MD5)}
}

func cleanRemote(name string) (string, error) {
	if !strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00") {
		return "", fmt.Errorf("网盘路径必须是以 / 开头的绝对路径: %q", name)
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("网盘路径不能包含 ..: %q", name)
		}
	}
	return path.Clean(name), nil
}

func driveRequest[T any](ctx context.Context, r *remote, call func(*api.Client) (T, error)) (T, error) {
	var zero T
	if time.Until(r.session.token.ExpiresAt) <= time.Minute {
		if err := r.session.refresh(ctx); err != nil {
			return zero, err
		}
		r.client = nil
	}
	if r.client == nil {
		r.client = r.session.newDrive(r.session.token.AccessToken)
	}
	value, err := call(r.client)
	if api.IsErrno(err, api.ErrnoAccessTokenExpired) || api.IsErrno(err, api.ErrnoAccessTokenInvalid) {
		if err := r.session.refresh(ctx); err != nil {
			return zero, err
		}
		r.client = r.session.newDrive(r.session.token.AccessToken)
		return call(r.client)
	}
	return value, err
}

func (r *remote) list(ctx context.Context, dir string) ([]*api.ListFileInfo, error) {
	const limit = 1000
	var files []*api.ListFileInfo
	seen := make(map[string]bool)
	for start := 0; ; {
		result, err := driveRequest(ctx, r, func(c *api.Client) (*api.ListResponse, error) {
			request, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			return c.File.List(request, &api.ListParams{
				Dir: dir, Order: api.Ptr("name"), Start: api.Ptr(start), Limit: api.Ptr(limit),
			})
		})
		if err != nil {
			return nil, fmt.Errorf("列出 %q: %w", dir, err)
		}
		for _, file := range result.List {
			if file == nil || path.Dir(file.Path) != dir || !validName(path.Base(file.Path)) ||
				file.Path != path.Join(dir, path.Base(file.Path)) || seen[file.Path] {
				return nil, fmt.Errorf("网盘目录 %q 返回了无效或重复的条目", dir)
			}
			seen[file.Path] = true
			files = append(files, file)
		}
		if len(result.List) < limit {
			return files, nil
		}
		start += len(result.List)
	}
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

func (r *remote) stat(ctx context.Context, name string) (*api.ListFileInfo, error) {
	if name == "/" {
		return &api.ListFileInfo{Path: "/", Isdir: 1}, nil
	}
	files, err := r.list(ctx, path.Dir(name))
	if api.IsErrno(err, api.ErrnoPathNotExist) || api.IsErrno(err, 31066) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.Path == name {
			return f, nil
		}
	}
	return nil, nil
}

func (r *remote) printList(ctx context.Context, out io.Writer, name string, recursive bool) error {
	item, err := r.stat(ctx, name)
	if err != nil {
		return err
	}
	if item == nil {
		return fmt.Errorf("网盘路径不存在: %q", name)
	}
	printFile := func(file *api.ListFileInfo) error {
		kind := "file"
		if file.Isdir == 1 {
			kind = "dir"
		}
		_, err := fmt.Fprintf(out, "%s\t%d\t%q\n", kind, file.Size, file.Path)
		return err
	}
	if item.Isdir != 1 {
		return printFile(item)
	}
	return r.walk(ctx, name, func(file *api.ListFileInfo) (bool, error) {
		return recursive, printFile(file)
	})
}

func (r *remote) walk(ctx context.Context, dir string, visit func(*api.ListFileInfo) (bool, error)) error {
	files, err := r.list(ctx, dir)
	if err != nil {
		return err
	}
	for _, file := range files {
		descend, err := visit(file)
		if err != nil {
			return err
		}
		if descend && file.Isdir == 1 {
			if err := r.walk(ctx, file.Path, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *remote) mkdir(ctx context.Context, dir string) error {
	_, err := driveRequest(ctx, r, func(c *api.Client) (*api.MkdirResponse, error) {
		request, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		return c.FileManager.Mkdir(request, &api.MkdirParams{Path: dir})
	})
	if api.IsErrno(err, api.ErrnoFileAlreadyExist) {
		file, statErr := r.stat(ctx, dir)
		if statErr == nil && file != nil && file.Isdir == 1 {
			return nil
		}
	}
	return err
}

func (r *remote) delete(ctx context.Context, file *api.ListFileInfo) error {
	// Recheck the object so another client replacing it during the sync does not
	// turn a planned deletion into a deletion of the replacement.
	current, err := r.stat(ctx, file.Path)
	if err != nil || current == nil {
		return err
	}
	if current.FsID != file.FsID || current.Isdir != file.Isdir ||
		(file.Isdir != 1 && version(current) != version(file)) {
		return errors.New("网盘条目在同步期间发生变化")
	}
	if file.Isdir == 1 {
		children, err := r.list(ctx, file.Path)
		if err != nil {
			return err
		}
		if len(children) != 0 {
			return errors.New("网盘目录在同步期间新增了条目")
		}
	}
	resp, err := driveRequest(ctx, r, func(c *api.Client) (*api.FileManagerResponse, error) {
		request, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		return c.FileManager.Delete(request, 0, []string{file.Path})
	})
	if err != nil {
		return err
	}
	for _, item := range resp.Info {
		if item != nil && item.Errno != 0 {
			return &api.APIError{Errno: item.Errno}
		}
	}
	return nil
}

const sliceSize = 4 * 1024 * 1024

func (r *remote) upload(ctx context.Context, target string, f io.ReaderAt, size int64, blocks []string) (remoteVersion, error) {
	// The Netdisk Open Platform's create and precreate endpoints use rtype=3
	// for replacement at the same path. Both requests must use the same value.
	rtype := api.Ptr(3)
	pre, err := driveRequest(ctx, r, func(c *api.Client) (*api.PrecreateResponse, error) {
		request, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		return c.Upload.Precreate(request, &api.PrecreateParams{Path: target, Size: size, BlockList: blocks, RType: rtype})
	})
	if err != nil {
		return remoteVersion{}, err
	}
	if pre.ReturnType == 2 && pre.UploadID == "" {
		file, err := r.stat(ctx, target)
		if err != nil {
			return remoteVersion{}, err
		}
		if file == nil || file.Isdir == 1 || file.Size != size || file.FsID == 0 {
			return remoteVersion{}, errors.New("无法确认秒传结果")
		}
		return version(file), nil
	}
	if pre.UploadID == "" {
		return remoteVersion{}, errors.New("预上传响应缺少 uploadid")
	}
	for _, part := range pre.BlockList {
		if part < 0 || part >= len(blocks) {
			return remoteVersion{}, fmt.Errorf("服务器返回无效分片序号 %d", part)
		}
		offset := int64(part) * sliceSize
		sendPart := func(c *api.Client) (*api.SliceUploadResponse, error) {
			request, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			return c.Upload.SliceUpload(request, &api.SliceUploadParams{
				Path: target, UploadID: pre.UploadID, PartSeq: part,
				File: io.NewSectionReader(f, offset, min(sliceSize, size-offset)),
			})
		}
		result, err := driveRequest(ctx, r, sendPart)
		for attempt := 0; attempt < 2 && transient(err); attempt++ {
			if err := sleepContext(ctx, time.Second<<attempt); err != nil {
				return remoteVersion{}, err
			}
			result, err = driveRequest(ctx, r, sendPart)
		}
		if err != nil {
			return remoteVersion{}, fmt.Errorf("上传分片 %d: %w", part, err)
		}
		if !strings.EqualFold(result.MD5, blocks[part]) {
			return remoteVersion{}, fmt.Errorf("分片 %d 校验失败，源文件可能已变化", part)
		}
	}
	// The server can omit blocks it already has. Create still needs the full,
	// ordered list, including blocks that did not need uploading.
	result, err := driveRequest(ctx, r, func(c *api.Client) (*api.CreateFileResponse, error) {
		request, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		return c.Upload.CreateFile(request, &api.CreateFileParams{
			Path: target, Size: size, UploadID: pre.UploadID, BlockList: blocks, RType: rtype,
		})
	})
	if err != nil {
		return remoteVersion{}, err
	}
	if result.FsID == 0 || result.Path != target || result.Size != size || result.Isdir != 0 {
		return remoteVersion{}, errors.New("网盘返回的上传结果与目标文件不一致")
	}
	return remoteVersion{result.FsID, result.Size, strings.ToLower(result.MD5)}, nil
}

func transient(err error) bool {
	var network *url.Error
	var response *api.APIError
	return errors.As(err, &network) ||
		errors.As(err, &response) && response.Response != nil &&
			(response.Response.StatusCode == http.StatusTooManyRequests || response.Response.StatusCode >= 500)
}

func (r *remote) download(ctx context.Context, file *api.ListFileInfo) (io.ReadCloser, error) {
	return driveRequest(ctx, r, func(c *api.Client) (io.ReadCloser, error) {
		request, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		res, err := c.Download.Meta(request, &api.MetaParams{FsIDs: []int64{file.FsID}})
		if err != nil {
			return nil, err
		}
		if len(res.List) != 1 || res.List[0].FsID != file.FsID || res.List[0].Size != file.Size ||
			res.List[0].Isdir != 0 || res.List[0].Path != file.Path ||
			(file.MD5 != "" && res.List[0].MD5 != "" && !strings.EqualFold(res.List[0].MD5, file.MD5)) {
			return nil, errors.New("网盘文件在同步期间发生变化")
		}
		link, err := url.Parse(res.List[0].Dlink)
		if err != nil || link.Scheme != "https" || !trustedBaiduHost(link.Hostname()) || link.User != nil {
			return nil, errors.New("网盘返回了无效下载链接")
		}
		body, length, err := c.Download.Download(ctx, &api.DownloadParams{Dlink: link.String()})
		if err != nil {
			return nil, err
		}
		if length >= 0 && length != file.Size {
			body.Close()
			return nil, fmt.Errorf("下载响应长度为 %d，预期 %d", length, file.Size)
		}
		return body, nil
	})
}
