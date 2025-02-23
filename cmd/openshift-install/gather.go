package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/openshift/installer/pkg/asset/installconfig"
	assetstore "github.com/openshift/installer/pkg/asset/store"
	"github.com/openshift/installer/pkg/gather/ssh"
	"github.com/openshift/installer/pkg/terraform"
	gatheraws "github.com/openshift/installer/pkg/terraform/gather/aws"
	gatherazure "github.com/openshift/installer/pkg/terraform/gather/azure"
	gatherlibvirt "github.com/openshift/installer/pkg/terraform/gather/libvirt"
	gatheropenstack "github.com/openshift/installer/pkg/terraform/gather/openstack"
	"github.com/openshift/installer/pkg/types"
	awstypes "github.com/openshift/installer/pkg/types/aws"
	azuretypes "github.com/openshift/installer/pkg/types/azure"
	libvirttypes "github.com/openshift/installer/pkg/types/libvirt"
	openstacktypes "github.com/openshift/installer/pkg/types/openstack"
)

func newGatherCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gather",
		Short: "Gather debugging data for a given installation failure",
		Long: `Gather debugging data for a given installation failure.

When installation for Openshift cluster fails, gathering all the data useful for debugging can
become a difficult task. This command helps users to collect the most relevant information that can be used
to debug the installation failures`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newGatherBootstrapCmd())
	return cmd
}

var (
	gatherBootstrapOpts struct {
		bootstrap string
		masters   []string
		sshKeys   []string
	}
)

func newGatherBootstrapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Gather debugging data for a failing-to-bootstrap control plane",
		Args:  cobra.ExactArgs(0),
		Run: func(_ *cobra.Command, _ []string) {
			cleanup := setupFileHook(rootOpts.dir)
			defer cleanup()
			err := runGatherBootstrapCmd(rootOpts.dir)
			if err != nil {
				logrus.Fatal(err)
			}
		},
	}
	cmd.PersistentFlags().StringVar(&gatherBootstrapOpts.bootstrap, "bootstrap", "", "Hostname or IP of the bootstrap host")
	cmd.PersistentFlags().StringArrayVar(&gatherBootstrapOpts.masters, "master", []string{}, "Hostnames or IPs of all control plane hosts")
	cmd.PersistentFlags().StringArrayVar(&gatherBootstrapOpts.sshKeys, "key", []string{}, "Path to SSH private keys that should be used for authentication. If no key was provided, SSH private keys from user's environment will be used")
	return cmd
}

func runGatherBootstrapCmd(directory string) error {
	tfStateFilePath := filepath.Join(directory, terraform.StateFileName)
	_, err := os.Stat(tfStateFilePath)
	if os.IsNotExist(err) {
		return unSupportedPlatformGather(directory)
	}
	if err != nil {
		return err
	}

	assetStore, err := assetstore.NewStore(directory)
	if err != nil {
		return errors.Wrap(err, "failed to create asset store")
	}

	config := &installconfig.InstallConfig{}
	if err := assetStore.Fetch(config); err != nil {
		return errors.Wrapf(err, "failed to fetch %s", config.Name())
	}

	tfstate, err := terraform.ReadState(tfStateFilePath)
	if err != nil {
		return errors.Wrapf(err, "failed to read state from %q", tfStateFilePath)
	}
	bootstrap, port, masters, err := extractHostAddresses(config.Config, tfstate)
	if err != nil {
		if err2, ok := err.(errUnSupportedGatherPlatform); ok {
			logrus.Error(err2)
			return unSupportedPlatformGather(directory)
		}
		return errors.Wrapf(err, "failed to get bootstrap and control plane host addresses from %q", tfStateFilePath)
	}

	return logGatherBootstrap(bootstrap, port, masters, directory)
}

func logGatherBootstrap(bootstrap string, port int, masters []string, directory string) error {
	logrus.Info("Pulling debug logs from the bootstrap machine")
	client, err := ssh.NewClient("core", fmt.Sprintf("%s:%d", bootstrap, port), gatherBootstrapOpts.sshKeys)
	if err != nil {
		return errors.Wrap(err, "failed to create SSH client")
	}
	if err := ssh.Run(client, fmt.Sprintf("/usr/local/bin/installer-gather.sh %s", strings.Join(masters, " "))); err != nil {
		return errors.Wrap(err, "failed to run remote command")
	}
	file := filepath.Join(directory, fmt.Sprintf("log-bundle-%s.tar.gz", time.Now().Format("20060102150405")))
	if err := ssh.PullFileTo(client, "/home/core/log-bundle.tar.gz", file); err != nil {
		return errors.Wrap(err, "failed to pull log file from remote")
	}
	logrus.Infof("Bootstrap gather logs captured here %q", file)
	return nil
}

func extractHostAddresses(config *types.InstallConfig, tfstate *terraform.State) (bootstrap string, port int, masters []string, err error) {
	port = 22
	switch config.Platform.Name() {
	case awstypes.Name:
		bootstrap, err = gatheraws.BootstrapIP(tfstate)
		if err != nil {
			return bootstrap, port, masters, err
		}
		masters, err = gatheraws.ControlPlaneIPs(tfstate)
		if err != nil {
			logrus.Error(err)
		}
	case azuretypes.Name:
		port = 2200
		bootstrap, err = gatherazure.BootstrapIP(tfstate)
		if err != nil {
			return bootstrap, port, masters, err
		}
		masters, err = gatherazure.ControlPlaneIPs(tfstate)
		if err != nil {
			logrus.Error(err)
		}
	case libvirttypes.Name:
		bootstrap, err = gatherlibvirt.BootstrapIP(tfstate)
		if err != nil {
			return bootstrap, port, masters, err
		}
		masters, err = gatherlibvirt.ControlPlaneIPs(tfstate)
		if err != nil {
			logrus.Error(err)
		}
	case openstacktypes.Name:
		bootstrap, err = gatheropenstack.BootstrapIP(tfstate)
		if err != nil {
			return bootstrap, port, masters, err
		}
		masters, err = gatheropenstack.ControlPlaneIPs(tfstate)
		if err != nil {
			logrus.Error(err)
		}
	default:
		return "", port, nil, errUnSupportedGatherPlatform{Message: fmt.Sprintf("Cannot fetch the bootstrap and control plane host addresses from state file for %s platform", config.Platform.Name())}
	}
	return bootstrap, port, masters, nil
}

type errUnSupportedGatherPlatform struct {
	Message string
}

func (e errUnSupportedGatherPlatform) Error() string {
	return e.Message
}

func unSupportedPlatformGather(directory string) error {
	if gatherBootstrapOpts.bootstrap == "" || len(gatherBootstrapOpts.masters) == 0 {
		return errors.New("boostrap host address and at least one control plane host address must be provided")
	}

	return logGatherBootstrap(gatherBootstrapOpts.bootstrap, 22, gatherBootstrapOpts.masters, directory)
}
