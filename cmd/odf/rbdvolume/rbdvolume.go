package rbdvolume

import (
	"github.com/red-hat-storage/odf-cli/cmd/odf/root"
	"github.com/red-hat-storage/odf-cli/pkg/rbd"
	"github.com/spf13/cobra"
)

var RBDVolumeCmd = &cobra.Command{
	Use:   "rbdvolume",
	Short: "Manages RBD volumes",
}

var listCmd = &cobra.Command{
	Use:     "ls",
	Short:   "Print the list of RBD volumes.",
	Example: "odf rbdvolume ls",
	Run: func(cmd *cobra.Command, args []string) {
		staleOnly, _ := cmd.Flags().GetBool("stale")
		poolName, _ := cmd.Flags().GetString("pool")

		r := &rbd.RBDVolume{
			Ctx:               cmd.Context(),
			Clientsets:        root.ClientSets,
			OperatorNamespace: root.OperatorNamespace,
			ClusterNamespace:  root.StorageClusterNamespace,
			PoolName:          poolName,
		}
		r.List(staleOnly)
	},
}

var deleteCmd = &cobra.Command{
	Use:     "delete",
	Short:   "Deletes an RBD volume or trash entry.",
	Args:    cobra.ExactArgs(1),
	Example: "odf rbdvolume delete <image-name-or-id>",
	Run: func(cmd *cobra.Command, args []string) {
		imageNameOrID := args[0]
		poolName, _ := cmd.Flags().GetString("pool")
		force, _ := cmd.Flags().GetBool("force")

		r := &rbd.RBDVolume{
			Ctx:               cmd.Context(),
			Clientsets:        root.ClientSets,
			OperatorNamespace: root.OperatorNamespace,
			ClusterNamespace:  root.StorageClusterNamespace,
			PoolName:          poolName,
		}
		r.Delete(imageNameOrID, force)
	},
}

func init() {
	RBDVolumeCmd.AddCommand(listCmd)
	RBDVolumeCmd.AddCommand(deleteCmd)

	RBDVolumeCmd.PersistentFlags().String("pool", "ocs-storagecluster-cephblockpool", "The name of the RBD pool")
	RBDVolumeCmd.PersistentFlags().Bool("stale", false, "Only list stale RBD volumes")

	deleteCmd.Flags().Bool("force", false, "Force delete by removing snapshots and flattening clones")
}

// Made with Bob
