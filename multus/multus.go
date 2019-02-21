// Copyright (c) 2017 Intel Corporation
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

// This is a "Multi-plugin".The delegate concept refered from CNI project
// It reads other plugin netconf, and then invoke them, e.g.
// flannel or sriov plugin.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	cniversion "github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ns"
	k8s "github.com/intel/multus-cni/k8sclient"
	"github.com/intel/multus-cni/logging"
	"github.com/intel/multus-cni/types"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/wait"
)

var version = "master@git"
var commit = "unknown commit"
var date = "unknown date"

var defaultReadinessBackoff = wait.Backoff{
	Steps:    4,
	Duration: 250 * time.Millisecond,
	Factor:   4.0,
	Jitter:   0.1,
}

func printVersionString() string {
	return fmt.Sprintf("multus-cni version:%s, commit:%s, date:%s",
		version, commit, date)
}

func saveScratchNetConf(containerID, dataDir string, netconf []byte) error {
	logging.Debugf("saveScratchNetConf: %s, %s, %s", containerID, dataDir, string(netconf))
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return logging.Errorf("failed to create the multus data directory(%q): %v", dataDir, err)
	}

	path := filepath.Join(dataDir, containerID)

	err := ioutil.WriteFile(path, netconf, 0600)
	if err != nil {
		return logging.Errorf("failed to write container data in the path(%q): %v", path, err)
	}

	return err
}

func consumeScratchNetConf(containerID, dataDir string) ([]byte, error) {
	logging.Debugf("consumeScratchNetConf: %s, %s", containerID, dataDir)
	path := filepath.Join(dataDir, containerID)
	defer os.Remove(path)

	return ioutil.ReadFile(path)
}

func getIfname(delegate *types.DelegateNetConf, argif string, idx int) string {
	logging.Debugf("getIfname: %v, %s, %d", delegate, argif, idx)
	if delegate.IfnameRequest != "" {
		return delegate.IfnameRequest
	}
	if delegate.MasterPlugin {
		// master plugin always uses the CNI-provided interface name
		return argif
	}

	// Otherwise construct a unique interface name from the delegate's
	// position in the delegate list
	return fmt.Sprintf("net%d", idx)
}

func saveDelegates(containerID, dataDir string, delegates []*types.DelegateNetConf) error {
	logging.Debugf("saveDelegates: %s, %s, %v", containerID, dataDir, delegates)
	delegatesBytes, err := json.Marshal(delegates)
	if err != nil {
		return logging.Errorf("error serializing delegate netconf: %v", err)
	}

	if err = saveScratchNetConf(containerID, dataDir, delegatesBytes); err != nil {
		return logging.Errorf("error in saving the  delegates : %v", err)
	}

	return err
}

func validateIfName(nsname string, ifname string) error {
	logging.Debugf("validateIfName: %s, %s", nsname, ifname)
	podNs, err := ns.GetNS(nsname)
	if err != nil {
		return logging.Errorf("no netns: %v", err)
	}

	err = podNs.Do(func(_ ns.NetNS) error {
		_, err := netlink.LinkByName(ifname)
		if err != nil {
			if err.Error() == "Link not found" {
				return nil
			}
			return err
		}
		return logging.Errorf("ifname %s is already exist", ifname)
	})

	return err
}

func conflistAdd(rt *libcni.RuntimeConf, rawnetconflist []byte, binDir string, exec invoke.Exec) (cnitypes.Result, error) {
	logging.Debugf("conflistAdd: %v, %s, %s", rt, string(rawnetconflist), binDir)
	// In part, adapted from K8s pkg/kubelet/dockershim/network/cni/cni.go
	binDirs := filepath.SplitList(os.Getenv("CNI_PATH"))
	binDirs = append(binDirs, binDir)
	cniNet := libcni.NewCNIConfig(binDirs, exec)

	confList, err := libcni.ConfListFromBytes(rawnetconflist)
	if err != nil {
		return nil, logging.Errorf("error in converting the raw bytes to conflist: %v", err)
	}

	result, err := cniNet.AddNetworkList(confList, rt)
	if err != nil {
		return nil, logging.Errorf("error in getting result from AddNetworkList: %v", err)
	}

	return result, nil
}

func conflistDel(rt *libcni.RuntimeConf, rawnetconflist []byte, binDir string, exec invoke.Exec) error {
	logging.Debugf("conflistDel: %v, %s, %s", rt, string(rawnetconflist), binDir)
	// In part, adapted from K8s pkg/kubelet/dockershim/network/cni/cni.go
	binDirs := filepath.SplitList(os.Getenv("CNI_PATH"))
	binDirs = append(binDirs, binDir)
	cniNet := libcni.NewCNIConfig(binDirs, exec)

	confList, err := libcni.ConfListFromBytes(rawnetconflist)
	if err != nil {
		return logging.Errorf("error in converting the raw bytes to conflist: %v", err)
	}

	err = cniNet.DelNetworkList(confList, rt)
	if err != nil {
		return logging.Errorf("error in getting result from DelNetworkList: %v", err)
	}

	return err
}

func delegateAdd(exec invoke.Exec, ifName string, delegate *types.DelegateNetConf, rt *libcni.RuntimeConf, binDir string, cniArgs string) (cnitypes.Result, error) {
	logging.Debugf("delegateAdd: %v, %s, %v, %v, %s", exec, ifName, delegate, rt, binDir)
	if os.Setenv("CNI_IFNAME", ifName) != nil {
		return nil, logging.Errorf("Multus: error in setting CNI_IFNAME")
	}

	if err := validateIfName(os.Getenv("CNI_NETNS"), ifName); err != nil {
		return nil, logging.Errorf("cannot set %q ifname to %q: %v", delegate.Conf.Type, ifName, err)
	}

	if delegate.MacRequest != "" || delegate.IPRequest != "" {
		if cniArgs != "" {
			cniArgs = fmt.Sprintf("%s;IgnoreUnknown=true", cniArgs)
		} else {
			cniArgs = "IgnoreUnknown=true"
		}
		if delegate.MacRequest != "" {
			// validate Mac address
			_, err := net.ParseMAC(delegate.MacRequest)
			if err != nil {
				return nil, logging.Errorf("failed to parse mac address %q", delegate.MacRequest)
			}

			cniArgs = fmt.Sprintf("%s;MAC=%s", cniArgs, delegate.MacRequest)
			logging.Debugf("Set MAC address %q to %q", delegate.MacRequest, ifName)
		}

		if delegate.IPRequest != "" {
			// validate IP address
			if strings.Contains(delegate.IPRequest, "/") {
				_, _, err := net.ParseCIDR(delegate.IPRequest)
				if err != nil {
					return nil, logging.Errorf("failed to parse CIDR %q", delegate.MacRequest)
				}
			} else if net.ParseIP(delegate.IPRequest) == nil {
				return nil, logging.Errorf("failed to parse IP address %q", delegate.IPRequest)
			}

			cniArgs = fmt.Sprintf("%s;IP=%s", cniArgs, delegate.IPRequest)
			logging.Debugf("Set IP address %q to %q", delegate.IPRequest, ifName)
		}
		if os.Setenv("CNI_ARGS", cniArgs) != nil {
			return nil, logging.Errorf("cannot set %q mac to %q and ip to %q", delegate.Conf.Type, delegate.MacRequest, delegate.IPRequest)
		}
	}

	if delegate.ConfListPlugin != false {
		result, err := conflistAdd(rt, delegate.Bytes, binDir, exec)
		if err != nil {
			return nil, logging.Errorf("Multus: error in invoke Conflist add - %q: %v", delegate.ConfList.Name, err)
		}

		return result, nil
	}

	result, err := invoke.DelegateAdd(delegate.Conf.Type, delegate.Bytes, exec)
	if err != nil {
		return nil, logging.Errorf("Multus: error in invoke Delegate add - %q: %v", delegate.Conf.Type, err)
	}

	return result, nil
}

func delegateDel(exec invoke.Exec, ifName string, delegateConf *types.DelegateNetConf, rt *libcni.RuntimeConf, binDir string) error {
	logging.Debugf("delegateDel: %v, %s, %v, %v, %s", exec, ifName, delegateConf, rt, binDir)
	if os.Setenv("CNI_IFNAME", ifName) != nil {
		return logging.Errorf("Multus: error in setting CNI_IFNAME")
	}

	if delegateConf.ConfListPlugin != false {
		err := conflistDel(rt, delegateConf.Bytes, binDir, exec)
		if err != nil {
			return logging.Errorf("Multus: error in invoke Conflist Del - %q: %v", delegateConf.ConfList.Name, err)
		}

		return err
	}

	if err := invoke.DelegateDel(delegateConf.Conf.Type, delegateConf.Bytes, exec); err != nil {
		return logging.Errorf("Multus: error in invoke Delegate del - %q: %v", delegateConf.Conf.Type, err)
	}

	return nil
}

func delPlugins(exec invoke.Exec, argIfname string, delegates []*types.DelegateNetConf, lastIdx int, rt *libcni.RuntimeConf, binDir string) error {
	logging.Debugf("delPlugins: %v, %s, %v, %d, %v, %s", exec, argIfname, delegates, lastIdx, rt, binDir)
	if os.Setenv("CNI_COMMAND", "DEL") != nil {
		return logging.Errorf("Multus: error in setting CNI_COMMAND to DEL")
	}

	var errorstrings []string
	for idx := lastIdx; idx >= 0; idx-- {
		ifName := getIfname(delegates[idx], argIfname, idx)
		rt.IfName = ifName
		// Attempt to delete all but do not error out, instead, collect all errors.
		if err := delegateDel(exec, ifName, delegates[idx], rt, binDir); err != nil {
			errorstrings = append(errorstrings, err.Error())
		}
	}

	// Check if we had any errors, and send them all back.
	if len(errorstrings) > 0 {
		return fmt.Errorf(strings.Join(errorstrings, " / "))
	}

	return nil
}

func cmdAdd(args *skel.CmdArgs, exec invoke.Exec, kubeClient k8s.KubeClient) (cnitypes.Result, error) {
	logging.Debugf("cmdAdd: %v, %v, %v", args, exec, kubeClient)
	n, err := types.LoadNetConf(args.StdinData)
	if err != nil {
		return nil, logging.Errorf("err in loading netconf: %v", err)
	}

	k8sArgs, err := k8s.GetK8sArgs(args)
	if err != nil {
		return nil, logging.Errorf("Multus: Err in getting k8s args: %v", err)
	}

	wait.ExponentialBackoff(defaultReadinessBackoff, func() (bool, error) {
		_, err := os.Stat(n.ReadinessIndicatorFile)
		switch {
		case err == nil:
			return true, nil
		default:
			return false, nil
		}
	})

	if n.ClusterNetwork != "" {
		err = k8s.GetDefaultNetworks(k8sArgs, n, kubeClient)
		if err != nil {
			return nil, logging.Errorf("Multus: Failed to get clusterNetwork/defaultNetworks: %v", err)
		}
		// First delegate is always the master plugin
		n.Delegates[0].MasterPlugin = true
	}

	numK8sDelegates, kc, err := k8s.TryLoadPodDelegates(k8sArgs, n, kubeClient)
	if err != nil {
		return nil, logging.Errorf("Multus: Err in loading K8s Delegates k8s args: %v", err)
	}

	if numK8sDelegates == 0 {
		// cache the multus config if we have only Multus delegates
		if err := saveDelegates(args.ContainerID, n.CNIDir, n.Delegates); err != nil {
			return nil, logging.Errorf("Multus: Err in saving the delegates: %v", err)
		}
	}

	var result, tmpResult cnitypes.Result
	var netStatus []*types.NetworkStatus
	cniArgs := os.Getenv("CNI_ARGS")
	for idx, delegate := range n.Delegates {
		ifName := getIfname(delegate, args.IfName, idx)
		rt := types.CreateCNIRuntimeConf(args, k8sArgs, ifName, n.RuntimeConfig)
		tmpResult, err = delegateAdd(exec, ifName, delegate, rt, n.BinDir, cniArgs)
		if err != nil {
			// If the add failed, tear down all networks we already added
			netName := delegate.Conf.Name
			if netName == "" {
				netName = delegate.ConfList.Name
			}
			// Ignore errors; DEL must be idempotent anyway
			_ = delPlugins(exec, args.IfName, n.Delegates, idx, rt, n.BinDir)
			return nil, logging.Errorf("Multus: Err adding pod to network %q: %v", netName, err)
		}

		// Master plugin result is always used if present
		if delegate.MasterPlugin || result == nil {
			result = tmpResult
		}

		//create the network status, only in case Multus as kubeconfig
		if n.Kubeconfig != "" && kc != nil {
			if !types.CheckSystemNamespaces(kc.Podnamespace, n.SystemNamespaces) {
				delegateNetStatus, err := types.LoadNetworkStatus(tmpResult, delegate.Conf.Name, delegate.MasterPlugin)
				if err != nil {
					return nil, logging.Errorf("Multus: Err in setting network status: %v", err)
				}

				netStatus = append(netStatus, delegateNetStatus)
			}
		}
	}

	//set the network status annotation in apiserver, only in case Multus as kubeconfig
	if n.Kubeconfig != "" && kc != nil {
		if !types.CheckSystemNamespaces(kc.Podnamespace, n.SystemNamespaces) {
			err = k8s.SetNetworkStatus(kc, netStatus)
			if err != nil {
				return nil, logging.Errorf("Multus: Err set the networks status: %v", err)
			}
		}
	}

	return result, nil
}

func cmdGet(args *skel.CmdArgs, exec invoke.Exec, kubeClient k8s.KubeClient) (cnitypes.Result, error) {
	logging.Debugf("cmdGet: %v, %v, %v", args, exec, kubeClient)
	in, err := types.LoadNetConf(args.StdinData)
	if err != nil {
		return nil, err
	}

	// FIXME: call all delegates

	return in.PrevResult, nil
}

func cmdDel(args *skel.CmdArgs, exec invoke.Exec, kubeClient k8s.KubeClient) error {
	os.Stderr.WriteString("!trace cmdDel TRACE A stderr\n")
	logging.Debugf("cmdDel: %v, %v, %v", args, exec, kubeClient)
	in, err := types.LoadNetConf(args.StdinData)
	logging.Debugf("!trace cmdDel: %v, %v, %v", args, exec, kubeClient)
	os.Stderr.WriteString("!trace cmdDel TRACE B stderr\n")
	if err != nil {
		return err
	}

	logging.Debugf("!trace TRACE C")

	if args.Netns == "" {
		return nil
	}

	logging.Debugf("!trace TRACE D")
	netns, err := ns.GetNS(args.Netns)
	logging.Debugf("!trace TRACE E: netns %s", netns)
	logging.Debugf("!trace TRACE E.1: err %s", err)
	if err != nil {

		logging.Debugf("!trace TRACE E.2: err %s", err)
		//  if NetNs is passed down by the Cloud Orchestration Engine, or if it called multiple times
		// so don't return an error if the device is already removed.
		// https://github.com/kubernetes/kubernetes/issues/43014#issuecomment-287164444
		foo, ok := err.(ns.NSPathNotExistErr)
		logging.Debugf("!trace TRACE E.3: foo %s", foo)
		logging.Debugf("!trace TRACE E.4: ok %s", ok)
		if ok {
			return nil
		}
		return fmt.Errorf("failed to open netns %q: %v", netns, err)
	}
	defer netns.Close()

	logging.Debugf("!trace TRACE F")
	k8sArgs, err := k8s.GetK8sArgs(args)
	if err != nil {
		return logging.Errorf("Multus: Err in getting k8s args: %v", err)
	}

	numK8sDelegates, kc, err := k8s.TryLoadPodDelegates(k8sArgs, in, kubeClient)
	if err != nil {
		return err
	}

	logging.Debugf("!trace TRACE G")

	if numK8sDelegates == 0 {
		// re-read the scratch multus config if we have only Multus delegates
		netconfBytes, err := consumeScratchNetConf(args.ContainerID, in.CNIDir)
		if err != nil {
			if os.IsNotExist(err) {
				// Per spec should ignore error if resources are missing / already removed
				return nil
			}
			return logging.Errorf("Multus: Err in  reading the delegates: %v", err)
		}

		if err := json.Unmarshal(netconfBytes, &in.Delegates); err != nil {
			return logging.Errorf("Multus: failed to load netconf: %v", err)
		}
	}

	logging.Debugf("!trace TRACE H")

	//unset the network status annotation in apiserver, only in case Multus as kubeconfig
	if in.Kubeconfig != "" && kc != nil {
		if !types.CheckSystemNamespaces(kc.Podnamespace, in.SystemNamespaces) {
			err := k8s.SetNetworkStatus(kc, nil)
			if err != nil {
				return logging.Errorf("Multus: Err unset the networks status: %v", err)
			}
		}
	}

	logging.Debugf("!trace TRACE I")

	rt := types.CreateCNIRuntimeConf(args, k8sArgs, "", in.RuntimeConfig)

	logging.Debugf("!trace TRACE J")
	return delPlugins(exec, args.IfName, in.Delegates, len(in.Delegates)-1, rt, in.BinDir)
}

func main() {

	// Init command line flags to clear vendored packages' one, especially in init()
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	// add version flag
	versionOpt := false
	flag.BoolVar(&versionOpt, "version", false, "Show application version")
	flag.BoolVar(&versionOpt, "v", false, "Show application version")
	flag.Parse()
	if versionOpt == true {
		fmt.Printf("%s\n", printVersionString())
		return
	}

	skel.PluginMain(
		func(args *skel.CmdArgs) error {
			result, err := cmdAdd(args, nil, nil)
			if err != nil {
				return err
			}
			return result.Print()
		},
		func(args *skel.CmdArgs) error {
			result, err := cmdGet(args, nil, nil)
			if err != nil {
				return err
			}
			return result.Print()
		},
		func(args *skel.CmdArgs) error {
			return cmdDel(args, nil, nil)
		},
		cniversion.All, "meta-plugin that delegates to other CNI plugins")
}
