package cli

import (
	"github.com/pkg/errors"
	"github.com/replicatedhq/kots/pkg/k8sutil"
	"github.com/replicatedhq/kots/pkg/logger"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func BackupPrintNFSConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "print-nfs-config",
		Short:         "Print snapshots current NFS configuration to use for setting up Velero",
		Long:          ``,
		SilenceUsage:  true,
		SilenceErrors: false,
		PreRun: func(cmd *cobra.Command, args []string) {
			viper.BindPFlags(cmd.Flags())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			v := viper.GetViper()

			namespace := v.GetString("namespace")
			if err := validateNamespace(namespace); err != nil {
				return err
			}

			clientset, err := k8sutil.GetClientset(kubernetesConfigFlags)
			if err != nil {
				return errors.Wrap(err, "failed to get clientset")
			}

			c, err := getNFSMinioVeleroConfig(cmd.Context(), clientset, namespace)
			if err != nil {
				return errors.Wrap(err, "failed to get nfs minio velero config")
			}

			log := logger.NewLogger()
			c.LogInfo(log)

			return nil
		},
	}

	cmd.Flags().StringP("namespace", "n", "", "the namespace in which kots/kotsadm is installed")

	return cmd
}
