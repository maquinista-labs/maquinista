package main

import (
	"fmt"
	"os"

	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/spf13/cobra"
)

var treeProject string

var treeCmd = &cobra.Command{
	Use:   "tree",
	Short: "Print dependency tree with status symbols",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		proj := treeProject
		if proj == "" {
			proj = os.Getenv("VOLTA_PROJECT")
		}
		var projPtr *string
		if proj != "" {
			projPtr = &proj
		}

		roots, err := db.GetDependencyTree(pool, projPtr)
		if err != nil {
			return err
		}

		if len(roots) == 0 {
			fmt.Println("No tasks.")
			return nil
		}

		for _, root := range roots {
			printTreeNode(root, "", true)
		}
		return nil
	},
}

func init() {
	treeCmd.Flags().StringVar(&treeProject, "project", "", "filter by project ID")
	rootCmd.AddCommand(treeCmd)
}

func printTreeNode(node *db.TreeNode, prefix string, isLast bool) {
	sym := statusSymbol(node.Task.Status)
	connector := "├── "
	if isLast {
		connector = "└── "
	}
	if prefix == "" {
		// Root node: no connector.
		fmt.Printf("  %s  %s  %s\n", sym, truncateID(node.Task.ID), node.Task.Title)
	} else {
		fmt.Printf("%s%s%s  %s  %s\n", prefix, connector, sym, truncateID(node.Task.ID), node.Task.Title)
	}

	childPrefix := prefix
	if prefix == "" {
		childPrefix = "  "
	} else if isLast {
		childPrefix = prefix + "    "
	} else {
		childPrefix = prefix + "│   "
	}

	for i, child := range node.Children {
		printTreeNode(child, childPrefix, i == len(node.Children)-1)
	}
}
