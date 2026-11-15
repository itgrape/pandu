package pandu

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baidu-netdisk/baidu-drive-sdk-go/baidudriver/api"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func reply(req *http.Request, status int, data string) (*http.Response, error) {
	return &http.Response{
		Request: req, StatusCode: status, Body: io.NopCloser(strings.NewReader(data)),
		ContentLength: int64(len(data)),
	}, nil
}

func testSession(t *testing.T, transport roundTripFunc) *session {
	t.Helper()
	s := &session{
		path:   filepath.Join(t.TempDir(), "config.json"),
		config: config{AppKey: "key", SecretKey: "secret"},
		token:  credentials{"token", "refresh", time.Now().Add(time.Hour), "test-session"},
	}
	s.newDrive = func(token string) *api.Client {
		return api.NewClient(api.WithHTTPClient(&http.Client{Transport: transport}), api.WithAccessToken(token))
	}
	s.auth = s.newDrive("").Auth
	return s
}

func TestPutIncremental(t *testing.T) {
	var id, precreates, creates, slices int
	var size int64
	var wantedBlocks []string
	s := testSession(t, func(req *http.Request) (*http.Response, error) {
		q := req.URL.Query()
		switch q.Get("method") {
		case "list":
			if id == 0 {
				return reply(req, 200, `{"list":[]}`)
			}
			return reply(req, 200, fmt.Sprintf(`{"list":[{"path":"/save","size":%d,"fs_id":%d,"md5":"opaque"}]}`, size, id))
		case "precreate":
			precreates++
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if req.Form.Get("rtype") != "3" || req.Form.Get("path") != "/save" {
				t.Fatalf("unexpected precreate form %v", req.Form)
			}
			if err := json.Unmarshal([]byte(req.Form.Get("block_list")), &wantedBlocks); err != nil || len(wantedBlocks) != 2 {
				t.Fatalf("unexpected block list %v, %v", wantedBlocks, err)
			}
			return reply(req, 200, `{"return_type":1,"uploadid":"upload","block_list":[1]}`)
		case "upload":
			slices++
			if q.Get("partseq") != "1" {
				t.Fatalf("uploaded unrequested block %v", q)
			}
			if err := req.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			defer req.MultipartForm.RemoveAll()
			part, _, err := req.FormFile("file")
			if err != nil {
				t.Fatal(err)
			}
			defer part.Close()
			data, err := io.ReadAll(part)
			if err != nil || len(data) != 1 {
				t.Fatalf("unexpected last block %v, %v", data, err)
			}
			return reply(req, 200, fmt.Sprintf(`{"md5":"%x"}`, md5.Sum(data)))
		case "create":
			creates++
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(wantedBlocks)
			if req.Form.Get("block_list") != string(want) || req.Form.Get("rtype") != "3" {
				t.Fatalf("unexpected create form %v", req.Form)
			}
			id++
			return reply(req, 200, fmt.Sprintf(`{"fs_id":%d,"path":"/save","size":%d,"md5":"opaque"}`, id, size))
		default:
			t.Fatalf("unexpected request %v", req.URL)
			return nil, nil
		}
	})

	name := filepath.Join(t.TempDir(), "save")
	data := make([]byte, sliceSize+1)
	size = int64(len(data))
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := openLocal(name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.root.Close()
	run := func(dry bool) string {
		var out strings.Builder
		if err := syncFiles(context.Background(), &out, s, l, "/save", true, SyncOptions{DryRun: dry}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	if out := run(true); !strings.Contains(out, "传输 1") || precreates != 0 {
		t.Fatalf("dry run sent writes or missed the transfer: %q", out)
	}
	if out := run(false); !strings.Contains(out, "传输 1") {
		t.Fatal(out)
	}
	if out := run(false); !strings.Contains(out, "跳过 1") || creates != 1 {
		t.Fatalf("same file transferred again: %q", out)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] = 42
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if out := run(false); !strings.Contains(out, "传输 1") || precreates != 2 || creates != 2 || slices != 2 {
		t.Fatalf("same-size content change missed or wrong blocks sent: %q", out)
	}
}

func TestGetPartialDownloadPreservesFiles(t *testing.T) {
	truncated := true
	downloads := 0
	s := testSession(t, func(req *http.Request) (*http.Response, error) {
		q := req.URL.Query()
		switch q.Get("method") {
		case "list":
			switch q.Get("dir") {
			case "/":
				return reply(req, 200, `{"list":[{"path":"/saves","isdir":1,"fs_id":1}]}`)
			case "/saves":
				return reply(req, 200, `{"list":[{"path":"/saves/save","size":4,"fs_id":2,"md5":"opaque"},{"path":"/saves/empty","isdir":1,"fs_id":3}]}`)
			case "/saves/empty":
				return reply(req, 200, `{"list":[]}`)
			}
		case "filemetas":
			return reply(req, 200, `{"list":[{"path":"/saves/save","size":4,"fs_id":2,"md5":"opaque","dlink":"https://d.pcs.baidu.com/get"}]}`)
		case "":
			if req.URL.Path == "/get" {
				downloads++
				if truncated {
					res, _ := reply(req, 200, "xy")
					res.ContentLength = 4
					return res, nil
				}
				return reply(req, 200, "save")
			}
		}
		t.Fatalf("unexpected request %v", req.URL)
		return nil, nil
	})

	target := t.TempDir()
	save, extra := filepath.Join(target, "save"), filepath.Join(target, "extra")
	for name, data := range map[string]string{save: "old!", extra: "keep"} {
		if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l, err := openLocal(target)
	if err != nil {
		t.Fatal(err)
	}
	defer l.root.Close()
	run := func(out io.Writer, options SyncOptions) error {
		return syncFiles(context.Background(), out, s, l, "/saves", false, options)
	}
	if err := run(io.Discard, SyncOptions{Delete: true}); err == nil {
		t.Fatal("short download succeeded")
	}
	if data, err := os.ReadFile(save); err != nil || !bytes.Equal(data, []byte("old!")) {
		t.Fatalf("previous save altered: %q, %v", data, err)
	}
	if _, err := os.Stat(extra); err != nil {
		t.Fatalf("deletion happened before the download completed: %v", err)
	}
	if files, err := filepath.Glob(filepath.Join(target, ".pandu-*.tmp")); err != nil || len(files) != 0 {
		t.Fatalf("partial download left files: %v, %v", files, err)
	}
	truncated = false
	if err := run(io.Discard, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(save); err != nil || string(data) != "save" {
		t.Fatalf("save was not restored: %q, %v", data, err)
	}
	if _, err := os.Stat(extra); err != nil {
		t.Fatalf("default run removed an extra file: %v", err)
	}
	var out strings.Builder
	if err := run(&out, SyncOptions{Delete: true}); err != nil {
		t.Fatal(err)
	}
	if downloads != 2 || !strings.Contains(out.String(), "跳过 1") {
		t.Fatalf("repeat get downloaded again: %q", out.String())
	}
	if _, err := os.Stat(extra); !os.IsNotExist(err) {
		t.Fatalf("extra file still present: %v", err)
	}
}

func TestRefreshPersistsRotatedToken(t *testing.T) {
	var listed bool
	s := testSession(t, func(req *http.Request) (*http.Response, error) {
		q := req.URL.Query()
		switch {
		case q.Get("grant_type") == "refresh_token":
			if q.Get("refresh_token") != "refresh" {
				t.Fatalf("wrong refresh token: %v", q)
			}
			return reply(req, 200, `{"access_token":"new","refresh_token":"rotated","expires_in":3600}`)
		case q.Get("method") == "list" && q.Get("access_token") == "new":
			listed = true
			return reply(req, 200, `{"list":[]}`)
		default:
			t.Fatalf("unexpected request %v", req.URL)
			return nil, nil
		}
	})
	s.token.ExpiresAt = time.Now().Add(-time.Minute)
	r := &remote{session: s}
	if _, err := r.list(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}
	var saved credentials
	if err := readJSON(s.path+".token.json", &saved); err != nil {
		t.Fatal(err)
	}
	if !listed || saved.AccessToken != "new" || saved.RefreshToken != "rotated" || saved.SessionID != "test-session" {
		t.Fatalf("new authorization not saved or used: %+v, %t", saved, listed)
	}
}
