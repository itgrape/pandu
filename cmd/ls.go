package cmd

import (
	"github.com/spf13/cobra"

	"pandu/internal/pandu"
)

func newListCommand(app *pandu.App) *cobra.Command {
	var recursive bool
	cmd := &cobra.Command{
		Use:               "ls [REMOTE]",
		Short:             "列出网盘文件或目录",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, args []string) error {
			remotePath := "/"
			if len(args) == 1 {
				remotePath = args[0]
			}
			return app.List(cmd.Context(), cmd.OutOrStdout(), remotePath, recursive)
		},
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "递归列出子目录")
	return cmd
}
