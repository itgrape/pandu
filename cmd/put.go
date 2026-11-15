package cmd

import (
	"github.com/spf13/cobra"

	"pandu/internal/pandu"
)

func newPutCommand(app *pandu.App) *cobra.Command {
	var options pandu.SyncOptions
	cmd := &cobra.Command{
		Use:               "put LOCAL REMOTE",
		Short:             "增量上传本地文件或目录",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeLocalArg(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.Put(cmd.Context(), cmd.OutOrStdout(), args[0], args[1], options)
		},
	}
	addSyncFlags(cmd, &options)
	return cmd
}
