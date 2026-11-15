package pandu

import (
	"context"
	"errors"
	"io"
)

// App runs backup and restore operations using the selected local configuration.
type App struct {
	ConfigPath string
}

func (app *App) Login(ctx context.Context, out io.Writer) error {
	s, release, err := openSession(app.ConfigPath)
	if err != nil {
		return err
	}
	defer release()
	return s.login(ctx, out)
}

func (app *App) List(ctx context.Context, out io.Writer, remotePath string, recursive bool) error {
	remotePath, err := cleanRemote(remotePath)
	if err != nil {
		return err
	}
	s, release, err := openSession(app.ConfigPath)
	if err != nil {
		return err
	}
	defer release()
	if err := s.loadToken(); err != nil {
		return err
	}
	return (&remote{session: s}).printList(ctx, out, remotePath, recursive)
}

func (app *App) Put(ctx context.Context, out io.Writer, localPath, remotePath string, options SyncOptions) error {
	return app.sync(ctx, out, localPath, remotePath, true, options)
}

func (app *App) Get(ctx context.Context, out io.Writer, remotePath, localPath string, options SyncOptions) error {
	return app.sync(ctx, out, localPath, remotePath, false, options)
}

func (app *App) sync(ctx context.Context, out io.Writer, localPath, remotePath string, upload bool, options SyncOptions) error {
	remotePath, err := cleanRemote(remotePath)
	if err != nil {
		return err
	}
	if localPath == "" {
		return errors.New("本地路径不能为空")
	}
	s, release, err := openSession(app.ConfigPath)
	if err != nil {
		return err
	}
	defer release()
	if err := s.loadToken(); err != nil {
		return err
	}
	local, err := openLocal(localPath)
	if err != nil {
		return err
	}
	defer local.root.Close()
	if err := s.checkSyncPath(local.absolute); err != nil {
		return err
	}
	return syncFiles(ctx, out, s, local, remotePath, upload, options)
}
