package cmd

import (
	"github.com/spf13/cobra"

	"pandu/internal/pandu"
)

func newLoginCommand(app *pandu.App) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "使用设备码登录百度网盘",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.Login(cmd.Context(), cmd.OutOrStdout())
		},
	}
}
