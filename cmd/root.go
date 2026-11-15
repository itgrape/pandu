package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"pandu/internal/pandu"
)

func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRootCommand().ExecuteContext(ctx)
	stop()
	if err != nil {
		var networkErr *url.Error
		if errors.As(err, &networkErr) {
			err = errors.New("百度网盘网络请求失败: " + networkErr.Err.Error())
		}
		fmt.Fprintln(os.Stderr, "pandu:", err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	app := &pandu.App{}
	root := &cobra.Command{
		Use:           "pandu",
		Short:         "备份文件到百度网盘，或从网盘恢复",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&app.ConfigPath, "config", os.Getenv("PANDU_CONFIG"), "JSON 配置路径（默认使用用户配置目录）")
	root.AddCommand(
		newLoginCommand(app),
		newListCommand(app),
		newPutCommand(app),
		newGetCommand(app),
	)
	return root
}
