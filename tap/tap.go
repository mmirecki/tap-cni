// Copyright 2022 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"fmt"
	"net"
	"runtime"

	"github.com/mmirecki/tap-cni/tap/conf"
	"github.com/mmirecki/tap-cni/tap/distro/rhel"

	"golang.org/x/sys/unix"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/vishvananda/netlink"

	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
	"github.com/containernetworking/plugins/pkg/utils/sysctl"
)

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func createLink(tmpName string, mtu int, nsFd int, multiqueue bool, mac string, owner int, group int) error {
	linkAttrs := netlink.LinkAttrs{
		Name:      tmpName,
		Namespace: netlink.NsFd(nsFd),
	}
	if mtu != 0 {
		linkAttrs.MTU = mtu
	}
	mv := &netlink.Tuntap{
		LinkAttrs: linkAttrs,
		Mode:      netlink.TUNTAP_MODE_TAP,
	}

	if owner >= 0 {
		mv.Owner = uint32(owner)
	}
	if group >= 0 {
		mv.Group = uint32(group)
	}

	if mac != "" {
		addr, err := net.ParseMAC(mac)
		if err != nil {
			return fmt.Errorf("invalid args %v for MAC addr: %v", mac, err)
		}
		linkAttrs.HardwareAddr = addr
	}
	if multiqueue {
		mv.Flags = netlink.TUNTAP_MULTI_QUEUE_DEFAULTS | netlink.TUNTAP_VNET_HDR | unix.IFF_TAP
	}
	if err := netlink.LinkAdd(mv); err != nil {
		return fmt.Errorf("failed to create tap: %v", err)
	}
	return nil
}

func createTap(conf *conf.NetConf, ifName string, netns ns.NetNS) (*current.Interface, error) {
	tap := &current.Interface{}
	// due to kernel bug we have to create with tmpName or it might
	// collide with the name on the host and error out
	tmpName, err := ip.RandomVethName()
	if err != nil {
		return nil, err
	}

	err = netns.Do(func(_ ns.NetNS) error {
		if conf.Distro != "" {
			if conf.Distro == "rhel" {
				err = rhel.Rhel.CreateLink(tmpName, conf, netns)
				if err != nil {
					return err
				}
			} else {
				return fmt.Errorf("No custom handling for distro %s defined ", conf.Distro)
			}
		} else {
			err := createLink(tmpName, conf.MTU, int(netns.Fd()), conf.MultiQueue, conf.Mac, conf.Owner, conf.Group)
			if err != nil {
				return err
			}
		}
		err = ip.RenameLink(tmpName, ifName)

		if err != nil {
			link, err := netlink.LinkByName(tmpName)
			if err != nil {
				netlink.LinkDel(link)
				return fmt.Errorf("failed to rename tap to %q: %v", ifName, err)
			}
		}
		tap.Name = ifName

		// Re-fetch link to get all properties/attributes
		link, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to refetch tap %q: %v", ifName, err)
		}

		err = netlink.LinkSetUp(link)
		if err != nil {
			return fmt.Errorf("failed to set tap interface up: %v", err)
		}

		tap.Mac = link.Attrs().HardwareAddr.String()
		tap.Sandbox = netns.Path()

		return nil
	})
	if err != nil {
		return nil, err
	}

	return tap, nil
}

func cmdAdd(args *skel.CmdArgs) error {
	n, cniVersion, err := conf.LoadConf(args)
	if err != nil {
		return err
	}

	isLayer3 := n.IPAM.Type != ""

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", netns, err)
	}
	defer netns.Close()

	tapInterface, err := createTap(n, args.IfName, netns)
	if err != nil {
		return err
	}

	// Delete link if err to avoid link leak in this ns
	defer func() {
		if err != nil {
			netns.Do(func(_ ns.NetNS) error {
				return ip.DelLinkByName(args.IfName)
			})
		}
	}()

	// Assume L2 interface only
	result := &current.Result{
		CNIVersion: cniVersion,
		Interfaces: []*current.Interface{tapInterface},
	}

	if isLayer3 {
		// run the IPAM plugin and get back the config to apply
		r, err := ipam.ExecAdd(n.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}

		// Invoke ipam del if err to avoid ip leak
		defer func() {
			if err != nil {
				ipam.ExecDel(n.IPAM.Type, args.StdinData)
			}
		}()

		// Convert whatever the IPAM result was into the current Result type
		ipamResult, err := current.NewResultFromResult(r)
		if err != nil {
			return err
		}

		if len(ipamResult.IPs) == 0 {
			return errors.New("IPAM plugin returned missing IP config")
		}

		result.IPs = ipamResult.IPs
		result.Routes = ipamResult.Routes

		for _, ipc := range result.IPs {
			// All addresses apply to the container tap interface
			ipc.Interface = current.Int(0)
		}

		err = netns.Do(func(_ ns.NetNS) error {
			_, _ = sysctl.Sysctl(fmt.Sprintf("net/ipv4/conf/%s/arp_notify", args.IfName), "1")

			if err := ipam.ConfigureIface(args.IfName, result); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			return err
		}
	} else {
		// For L2 just change interface status to up
		err = netns.Do(func(_ ns.NetNS) error {
			tapInterfaceLink, err := netlink.LinkByName(args.IfName)
			if err != nil {
				return fmt.Errorf("failed to find interface name %q: %v", tapInterface.Name, err)
			}
			if err := netlink.LinkSetUp(tapInterfaceLink); err != nil {
				return fmt.Errorf("failed to set %q UP: %v", args.IfName, err)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	result.DNS = n.DNS
	return types.PrintResult(result, cniVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	n, _, err := conf.LoadConf(args)
	if err != nil {
		return err
	}

	isLayer3 := n.IPAM.Type != ""

	if isLayer3 {
		err = ipam.ExecDel(n.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}
	}

	if args.Netns == "" {
		return nil
	}

	// There is a netns so try to clean up. Delete can be called multiple times
	// so don't return an error if the device is already removed.
	err = ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		if err := ip.DelLinkByName(args.IfName); err != nil {
			if err != ip.ErrLinkNotFound {
				return err
			}
		}
		return nil
	})

	if err != nil {
		//  if NetNs is passed down by the Cloud Orchestration Engine, or if it called multiple times
		// so don't return an error if the device is already removed.
		// https://github.com/kubernetes/kubernetes/issues/43014#issuecomment-287164444
		_, ok := err.(ns.NSPathNotExistErr)
		if ok {
			return nil
		}
		return err
	}

	return err
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("tap"))
}

func cmdCheck(args *skel.CmdArgs) error {
	n, _, err := conf.LoadConf(args)
	if err != nil {
		return err
	}
	isLayer3 := n.IPAM.Type != ""

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	if isLayer3 {
		// run the IPAM plugin and get back the config to apply
		err = ipam.ExecCheck(n.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}
	}

	// Parse previous result.
	if n.NetConf.RawPrevResult == nil {
		return fmt.Errorf("Required prevResult missing")
	}

	if err := version.ParsePrevResult(&n.NetConf); err != nil {
		return err
	}

	result, err := current.NewResultFromResult(n.PrevResult)
	if err != nil {
		return err
	}

	var contMap current.Interface
	// Find interfaces for names whe know, tap device name inside container
	for _, intf := range result.Interfaces {
		if args.IfName == intf.Name {
			if args.Netns == intf.Sandbox {
				contMap = *intf
				continue
			}
		}
	}

	// The namespace must be the same as what was configured
	if args.Netns != contMap.Sandbox {
		return fmt.Errorf("Sandbox in prevResult %s doesn't match configured netns: %s",
			contMap.Sandbox, args.Netns)
	}

	// Check prevResults for ips, routes and dns against values found in the container
	if err := netns.Do(func(_ ns.NetNS) error {
		err = ip.ValidateExpectedInterfaceIPs(args.IfName, result.IPs)
		if err != nil {
			return err
		}

		err = ip.ValidateExpectedRoute(result.Routes)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	return nil
}
