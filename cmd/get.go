package cmd

import (
	"github.com/spf13/cobra"

	"pandu/internal/pandu"
)

func newGetCommand(app *pandu.App) *cobra.Command {
	var options pandu.SyncOptions
	cmd := &cobra.Command{
		Use:               "get REMOTE LOCAL",
		Short:             "增量下载网盘文件或目录",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeLocalArg(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.Get(cmd.Context(), cmd.OutOrStdout(), args[0], args[1], options)
		},
	}
	addSyncFlags(cmd, &options)
	return cmd
}
