package pandu

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/baidu-netdisk/baidu-drive-sdk-go/baidudriver/api"
	"github.com/gofrs/flock"
)

type config struct {
	AppKey    string `json:"app_key"`
	SecretKey string `json:"secret_key"`
}

type credentials struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	SessionID    string    `json:"session_id"`
}

type session struct {
	config
	token    credentials
	auth     *api.AuthService
	newDrive func(string) *api.Client
	path     string
}

func openSession(name string) (*session, func(), error) {
	if name == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return nil, nil, err
		}
		name = filepath.Join(dir, "pandu", "config.json")
	}
	name, err := filepath.Abs(name)
	if err != nil {
		return nil, nil, err
	}
	// Aliases through a symlink must share the same token, state, and lock files.
	if resolved, err := filepath.EvalSymlinks(name); err == nil {
		name = resolved
	}
	s := &session{path: name}
	if err := readJSON(name, &s.config); err != nil {
		return nil, nil, fmt.Errorf("读取应用配置: %w", err)
	}
	if strings.TrimSpace(s.AppKey) == "" || strings.TrimSpace(s.SecretKey) == "" {
		return nil, nil, errors.New("请在配置文件中填写 app_key 和 secret_key")
	}
	lock := flock.New(name + ".lock")
	locked, err := lock.TryLock()
	if err != nil {
		return nil, nil, fmt.Errorf("锁定配置: %w", err)
	}
	if !locked {
		return nil, nil, errors.New("另一个 pandu 进程正在使用此配置")
	}
	s.auth = newSDKClient("").Auth
	s.newDrive = newSDKClient
	return s, func() { _ = lock.Close() }, nil
}

func newSDKClient(token string) *api.Client {
	hc := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			u := req.URL
			if len(via) >= 10 || u.Scheme != "https" ||
				!trustedBaiduHost(u.Hostname()) || via[0].URL.Hostname() == "openapi.baidu.com" {
				return errors.New("百度网盘请求被重定向到不受支持的地址")
			}
			return nil
		},
	}
	return api.NewClient(api.WithHTTPClient(hc), api.WithAccessToken(token))
}

func trustedBaiduHost(host string) bool {
	return strings.HasSuffix(strings.ToLower(host), ".baidu.com")
}

func (s *session) loadToken() error {
	if err := readJSON(s.path+".token.json", &s.token); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errors.New("请先运行 pandu login")
		}
		return fmt.Errorf("读取授权: %w", err)
	}
	if s.token.AccessToken == "" || s.token.RefreshToken == "" || s.token.SessionID == "" {
		return errors.New("授权文件不完整，请重新运行 pandu login")
	}
	return nil
}

func (s *session) saveToken(access, refresh string, lifetime int, id string, issuedAt time.Time) error {
	if access == "" || refresh == "" || lifetime <= 0 {
		return errors.New("授权响应不完整")
	}
	token := credentials{access, refresh, issuedAt.Add(time.Duration(lifetime) * time.Second), id}
	if err := writeJSON(s.path+".token.json", &token); err != nil {
		return fmt.Errorf("保存授权: %w", err)
	}
	s.token = token
	return nil
}

func (s *session) login(ctx context.Context, out io.Writer) error {
	request, cancel := context.WithTimeout(ctx, 30*time.Second)
	device, err := s.auth.DeviceCode(request, s.AppKey)
	cancel()
	if err != nil {
		return fmt.Errorf("获取设备码: %w", err)
	}
	link, err := url.Parse(device.VerificationURL)
	if err != nil || !trustedBaiduHost(link.Hostname()) || link.User != nil || (link.Scheme != "http" && link.Scheme != "https") || device.ExpiresIn <= 0 || device.DeviceCode == "" || device.UserCode == "" {
		return errors.New("设备授权响应不完整")
	}
	link.Scheme = "https"
	query := link.Query()
	query.Set("code", device.UserCode)
	link.RawQuery = query.Encode()
	if _, err := fmt.Fprintf(out, "请在有浏览器的设备上打开 %s\n授权码 %s\n等待授权确认…\n", link, device.UserCode); err != nil {
		return err
	}

	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(device.ExpiresIn)*time.Second)
	defer cancel()
	interval := time.Duration(max(device.Interval, 3)) * time.Second
	for {
		if err := sleepContext(pollCtx, interval); err != nil {
			if ctx.Err() == nil {
				return errors.New("设备码已过期，请重新运行 pandu login")
			}
			return err
		}
		started := time.Now()
		request, cancel := context.WithTimeout(pollCtx, 30*time.Second)
		token, err := s.auth.DeviceToken(request, s.AppKey, s.SecretKey, device.DeviceCode)
		cancel()
		if err == nil {
			if err := s.saveToken(token.AccessToken, token.RefreshToken, token.ExpiresIn, rand.Text(), started); err != nil {
				return err
			}
			_, err := fmt.Fprintln(out, "授权已保存")
			return err
		}
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Errno == -1 {
			code, _, _ := strings.Cut(apiErr.Errmsg, ":")
			switch code {
			case "", "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			}
		}
		return fmt.Errorf("设备授权失败: %w", err)
	}
}

func (s *session) refresh(ctx context.Context) error {
	started := time.Now()
	request, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	token, err := s.auth.RefreshToken(request, s.AppKey, s.SecretKey, s.token.RefreshToken)
	if err != nil {
		return fmt.Errorf("刷新授权失败（若授权失效，请运行 pandu login）: %w", err)
	}
	refresh := token.RefreshToken
	if refresh == "" {
		refresh = s.token.RefreshToken
	}
	return s.saveToken(token.AccessToken, refresh, token.ExpiresIn, s.token.SessionID, started)
}

func readJSON(name string, value any) error {
	data, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func writeJSON(name string, value any) error {
	f, err := os.CreateTemp(filepath.Dir(name), ".pandu-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := json.NewEncoder(f).Encode(value); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), name)
}

func (s *session) checkSyncPath(local string) error {
	// Existing path components may have aliases through the user's filesystem.
	resolved, err := resolveLocal(local)
	if err != nil {
		return err
	}
	for _, suffix := range []string{"", ".lock", ".token.json", ".sync.json"} {
		rel, err := filepath.Rel(resolved, s.path+suffix)
		if err == nil && filepath.IsLocal(rel) {
			return errors.New("同步路径包含 pandu 正在使用的配置或状态文件")
		}
	}
	return nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
