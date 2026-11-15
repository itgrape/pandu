package cmd

import (
	"github.com/spf13/cobra"

	"pandu/internal/pandu"
)

func addSyncFlags(cmd *cobra.Command, options *pandu.SyncOptions) {
	cmd.Flags().BoolVarP(&options.DryRun, "dry-run", "n", false, "仅显示计划，不修改同步目标")
	cmd.Flags().BoolVar(&options.Delete, "delete", false, "删除目录目标中源端没有的条目")
}

func completeLocalArg(position int) cobra.CompletionFunc {
	return func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) == position {
			return nil, cobra.ShellCompDirectiveDefault
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}
