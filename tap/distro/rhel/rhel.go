package rhel

import (
	"fmt"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/mmirecki/tap-cni/tap/conf"
	"github.com/mmirecki/tap-cni/tap/distro"
	"github.com/opencontainers/selinux/go-selinux"

	"os"
	"os/exec"
	"strconv"
	"syscall"
)

var Rhel distro.Distro = CreateLink{}

type CreateLink struct{}

func (l CreateLink) CreateLink(tmpName string, conf *conf.NetConf, netns ns.NetNS) error {
	//func (l CreateLink) CreateLink(tmpName string, mtu int, nsFd int, nsPath string, multique bool, mac string, owner int, group int, securityContext string) error {

	err := setContainerSeBool()
	if err != nil {
		return err
	}
	err = createSelinuxTap(tmpName, conf, netns)
	if err != nil {
		return err
	}
	return nil
}

func getSeBoolValue(sebool string) bool {
	file, err := os.ReadFile(sebool)
	if err != nil {
		return false
	}
	return len(file) > 0 && file[0] == '1'
}

func setContainerSeBool() error {
	if getSeBoolValue("/sys/fs/selinux/booleans/container_use_devices") {
		output, err := exec.Command("setsebool", "-P", "container_use_devices", "true").CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to run setsebool command %s: %v", string(output), err)
		}
	}
	return nil
}

// Due to issues with the vishvananda/netlink library which does not allow to create taps with no owner/group (fix pending)
// this method is using the ip tool to set up the tap device. Once the netlink library is fixed this can be changed
// to using the netlink library. Note that even after changing the code to use the netlink lib the selinux context
// still has to be changed, so it would have to be executed in a new process.
func createSelinuxTap(tmpName string, conf *conf.NetConf, netns ns.NetNS) error {
	if conf.SelinuxContext != "" {
		if err := selinux.SetExecLabel(conf.SelinuxContext); err != nil {
			return fmt.Errorf("failed set socket label: %v", err)
		}
	}
	minFDToCloseOnExec := 3
	maxFDToCloseOnExec := 256
	// we want to share the parent process std{in|out|err} - fds 0 through 2.
	// Since the FDs are inherited on fork / exec, we close on exec all others.
	for fd := minFDToCloseOnExec; fd < maxFDToCloseOnExec; fd++ {
		syscall.CloseOnExec(fd)
	}

	tapDeviceArgs := []string{"tuntap", "add", "mode", "tap", "name", tmpName}
	if conf.MultiQueue {
		tapDeviceArgs = append(tapDeviceArgs, "multi_queue")
	}

	if conf.Owner >= 0 {
		tapDeviceArgs = append(tapDeviceArgs, "user", strconv.Itoa(conf.Owner))
	}
	if conf.Group >= 0 {
		tapDeviceArgs = append(tapDeviceArgs, "group", strconv.Itoa(conf.Group))
	}
	output, err := exec.Command("ip", tapDeviceArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to run command %s: %v", output, err)
	}

	tapDeviceArgs = []string{"link", "set", tmpName}
	if conf.MTU != 0 {
		tapDeviceArgs = append(tapDeviceArgs, "mtu", strconv.Itoa(conf.MTU))
	}
	if conf.Mac != "" {
		tapDeviceArgs = append(tapDeviceArgs, "address", conf.Mac)
	}
	output, err = exec.Command("ip", tapDeviceArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to run command %s: %v", output, err)
	}
	return nil
}
