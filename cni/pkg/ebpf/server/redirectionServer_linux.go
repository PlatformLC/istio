// Copyright Istio Authors
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

package server

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpf -cflags "-D__TARGET_ARCH_x86"  ambient_redirect ../app/ambient_redirect.bpf.c
//go:generate sh -c "echo '// Copyright Istio Authors' > banner.tmp"
//go:generate sh -c "echo '//' >> banner.tmp"
//go:generate sh -c "echo '// Licensed under the Apache License, Version 2.0 (the \"License\");' >> banner.tmp"
//go:generate sh -c "echo '// you may not use this file except in compliance with the License.' >> banner.tmp"
//go:generate sh -c "echo '// You may obtain a copy of the License at' >> banner.tmp"
//go:generate sh -c "echo '//' >> banner.tmp"
//go:generate sh -c "echo '//     http://www.apache.org/licenses/LICENSE-2.0' >> banner.tmp"
//go:generate sh -c "echo '//' >> banner.tmp"
//go:generate sh -c "echo '// Unless required by applicable law or agreed to in writing, software' >> banner.tmp"
//go:generate sh -c "echo '// distributed under the License is distributed on an \"AS IS\" BASIS,' >> banner.tmp"
//go:generate sh -c "echo '// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.' >> banner.tmp"
//go:generate sh -c "echo '// See the License for the specific language governing permissions and' >> banner.tmp"
//go:generate sh -c "echo '// limitations under the License.\n' >> banner.tmp"
//go:generate sh -c "cat banner.tmp ambient_redirect_bpf.go > tmp.go && mv tmp.go ambient_redirect_bpf.go && rm banner.tmp"

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/cilium/ebpf"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/florianl/go-tc"
	"github.com/florianl/go-tc/core"
	"github.com/hashicorp/go-multierror"
	"github.com/josharian/native"
	"golang.org/x/sys/unix"

	"istio.io/istio/pkg/util/istiomultierror"
	istiolog "istio.io/pkg/log"
)

var log = istiolog.RegisterScope("ebpf", "ambient ebpf", 0)

const (
	FilesystemTypeBPFFS = unix.BPF_FS_MAGIC
	MapsRoot            = "/sys/fs/bpf"
	MapsPinpath         = "/sys/fs/bpf/ambient"
	CaptureDNSFlag      = uint8(1 << 0)

	QdiscKind            = "clsact"
	TcaBpfFlagActDiretct = uint32(1 << 0) // refer to include/uapi/linux/pkt_cls.h TCA_BPF_FLAG_ACT_DIRECT
	TcPrioFilter         = 1              // refer to include/uapi/linux/pkt_sched.h TC_PRIO_FILLER
)

const (
	EBPFLogLevelNone uint32 = iota
	EBPFLogLevelInfo
	EBPFLogLevelDebug
)

var isBigEndian = native.IsBigEndian

type TCFilterDir string

const (
	IngressDir TCFilterDir = "ingress"
	EgressDir  TCFilterDir = "egress"
)

type RedirectServer struct {
	redirectArgsChan           chan *RedirectArgs
	obj                        ambient_redirectObjects
	ztunnelHostingressFd       uint32
	ztunnelHostingressProgName string
	ztunnelIngressFd           uint32
	ztunnelIngressProgName     string
	inboundFd                  uint32
	inboundProgName            string
	outboundFd                 uint32
	outboundProgName           string
}

var stringToLevel = map[string]uint32{
	"debug": EBPFLogLevelDebug,
	"info":  EBPFLogLevelInfo,
	"none":  EBPFLogLevelNone,
}

func (r *RedirectServer) SetLogLevel(level string) {
	if err := r.obj.LogLevel.Update(uint32(0), stringToLevel[level], ebpf.UpdateAny); err != nil {
		log.Errorf("failed to update ebpf log level: %v", err)
	}
}

func (r *RedirectServer) UpdateHostIP(ips []string) error {
	if len(ips) > 2 {
		return fmt.Errorf("too may ips inputed: %d", len(ips))
	}
	for _, v := range ips {
		ip, err := netip.ParseAddr(v)
		if err != nil {
			return err
		}
		if ip.Is4() {
			err = r.obj.HostIpInfo.Update(uint32(0), ip.As16(), ebpf.UpdateAny)
		} else {
			err = r.obj.HostIpInfo.Update(uint32(1), ip.As16(), ebpf.UpdateAny)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func AddPodToMesh(ifIndex uint32, macAddr net.HardwareAddr, ips []netip.Addr) error {
	r := RedirectServer{}

	if err := setLimit(); err != nil {
		return err
	}

	if err := r.initBpfObjects(); err != nil {
		return err
	}

	defer r.obj.Close()

	multiErr := istiomultierror.New()

	if err := r.attachTCForWorkLoad(ifIndex); err != nil {
		multiErr = multierror.Append(multiErr, err)
		if err := r.detachTCForWorkload(ifIndex); err != nil {
			multiErr = multierror.Append(multiErr, err)
		}
		return multiErr.ErrorOrNil()
	}
	mapInfo := mapInfo{
		Ifindex: ifIndex,
	}
	if len(macAddr) != 6 {
		return fmt.Errorf("invalid mac addr(%s), only EUI-48/MAC-48 is supported", macAddr.String())
	}
	copy(mapInfo.MacAddr[:], macAddr)

	if len(ips) == 0 {
		return fmt.Errorf("nil ips inputed")
	}
	// TODO: support multiple IPs and IPv6
	ipAddr := ips[0]
	// ip slice is just in network endian
	ip := ipAddr.AsSlice()
	if len(ip) != 4 {
		return fmt.Errorf("invalid ip addr(%s), ipv4 is supported", ipAddr.String())
	}
	if err := r.obj.AppInfo.Update(ip, mapInfo, ebpf.UpdateAny); err != nil {
		multiErr = multierror.Append(multiErr, err)
		if err := r.detachTCForWorkload(ifIndex); err != nil {
			multiErr = multierror.Append(multiErr, err)
		}
	}

	return multiErr.ErrorOrNil()
}

func (r *RedirectServer) initBpfObjects() error {
	var options ebpf.CollectionOptions
	if _, err := os.Stat(MapsPinpath); err != nil {
		if os.IsNotExist(err) {
			if err := os.Mkdir(MapsPinpath, os.ModePerm); err != nil {
				return fmt.Errorf("unable to create ambient bpf mount directory: %v", err)
			}
		}
	}
	options.Maps.PinPath = MapsPinpath
	// load ebpf program
	obj := ambient_redirectObjects{}
	if err := loadAmbient_redirectObjects(&obj, &options); err != nil {
		return fmt.Errorf("loading objects: %v", err)
	}
	r.obj = obj
	r.ztunnelHostingressFd = uint32(r.obj.ZtunnelHostIngress.FD())
	ztunnelHostingressInfo, err := r.obj.ZtunnelHostIngress.Info()
	if err != nil {
		return fmt.Errorf("unable to load metadata of bfp prog: %v", err)
	}
	r.ztunnelHostingressProgName = ztunnelHostingressInfo.Name
	r.ztunnelIngressFd = uint32(r.obj.ZtunnelIngress.FD())
	ztunnelIngressInfo, err := r.obj.ZtunnelIngress.Info()
	if err != nil {
		return fmt.Errorf("unable to load metadata of bfp prog: %v", err)
	}
	r.ztunnelIngressProgName = ztunnelIngressInfo.Name

	r.inboundFd = uint32(r.obj.AppInbound.FD())
	inboundInfo, err := r.obj.AppInbound.Info()
	if err != nil {
		return fmt.Errorf("unable to load metadata of bfp prog: %v", err)
	}
	r.inboundProgName = inboundInfo.Name
	r.outboundFd = uint32(r.obj.AppOutbound.FD())
	outboundInfo, err := r.obj.AppOutbound.Info()
	if err != nil {
		return fmt.Errorf("unable to load metadata of bfp prog: %v", err)
	}
	r.outboundProgName = outboundInfo.Name
	return nil
}

// Note: this struct should be exactly the same defined in C
// it will be encoded byte by byte into memory
type mapInfo struct {
	Ifindex uint32
	MacAddr [6]byte
	Flag    uint8
	Pad     uint8
}

func NewRedirectServer() *RedirectServer {
	if err := checkOrMountBPFFSDefault(); err != nil {
		log.Fatalf("BPF filesystem mounting on /sys/fs/bpf failed: %v", err)
	}

	if err := setLimit(); err != nil {
		log.Fatalf("Setting limit failed: %v", err)
	}

	r := &RedirectServer{
		redirectArgsChan: make(chan *RedirectArgs),
	}

	if err := r.initBpfObjects(); err != nil {
		log.Fatalf("Init bpf objects failed: %v", err)
	}

	return r
}

func checkOrMountBPFFSDefault() error {
	var err error

	_, err = os.Stat(MapsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(MapsRoot, 0o755); err != nil {
				return fmt.Errorf("unable to create bpf mount directory: %s", err)
			}
		}
	}

	fst := unix.Statfs_t{}
	err = unix.Statfs(MapsRoot, &fst)
	if err != nil {
		return &os.PathError{Op: "statfs", Path: MapsRoot, Err: err}
	} else if fst.Type == FilesystemTypeBPFFS {
		return nil
	}

	err = unix.Mount(MapsRoot, MapsRoot, "bpf", 0, "")
	if err != nil {
		return fmt.Errorf("failed to mount %s: %s", MapsRoot, err)
	}

	return nil
}

func setLimit() error {
	return unix.Setrlimit(unix.RLIMIT_MEMLOCK,
		&unix.Rlimit{
			Cur: unix.RLIM_INFINITY,
			Max: unix.RLIM_INFINITY,
		})
}

func (r *RedirectServer) Start(stop <-chan struct{}) {
	log.Infof("Starting redirection Server")
	go func() {
		for {
			select {
			case arg := <-r.redirectArgsChan:
				if err := r.handleRequest(arg); err != nil {
					log.Errorf("failed to handle request: %v", err)
				}

			case <-stop:
				r.obj.Close()
				return
			}
		}
	}()
}

func (r *RedirectServer) handleRequest(args *RedirectArgs) error {
	var mapInfo mapInfo
	multiErr := istiomultierror.New()
	ipAddrs := args.IPAddrs
	macAddr := args.MacAddr
	ifindex := uint32(args.Ifindex)
	peerIndex := uint32(args.PeerIndex)
	ztunnel := args.IsZtunnel
	namespace := args.PeerNs
	remove := args.Remove

	if !remove {
		if len(macAddr) != 6 {
			return fmt.Errorf("invalid mac addr(%s), only EUI-48/MAC-48 is supported", macAddr.String())
		}
		mapInfo.Ifindex = ifindex
		copy(mapInfo.MacAddr[:], macAddr)
	}

	if ztunnel {
		if remove {
			if ifindex != 0 && namespace != "" {
				if err := r.detachTCForZtunnel(ifindex, peerIndex, namespace); err != nil {
					multiErr = multierror.Append(multiErr, err)
				}
			} else {
				log.Debugf("ifindex(%d) or namespace(%s) invalid for ztunnel removal", ifindex, namespace)
			}
			// For array map, kernel doesn't support delete elem(refer to kernel/bpf/arraymap.c)
			// it works just like an 'array'.
			if err := r.obj.ZtunnelInfo.Update(uint32(0), mapInfo, ebpf.UpdateAny); err != nil {
				multiErr = multierror.Append(multiErr, err)
			}
		} else {
			if namespace == "" {
				return fmt.Errorf("invalid namespace")
			}
			if err := r.attachTCForZtunnel(ifindex, peerIndex, namespace); err != nil {
				multiErr = multierror.Append(multiErr, err)
				if err := r.detachTCForZtunnel(ifindex, peerIndex, namespace); err != nil {
					multiErr = multierror.Append(multiErr, err)
				}
				return multiErr.ErrorOrNil()
			}
			if args.CaptureDNS {
				mapInfo.Flag |= CaptureDNSFlag
			}
			if err := r.obj.ZtunnelInfo.Update(uint32(0), mapInfo, ebpf.UpdateAny); err != nil {
				multiErr = multierror.Append(multiErr, err)
				if err := r.detachTCForZtunnel(ifindex, peerIndex, namespace); err != nil {
					multiErr = multierror.Append(multiErr, err)
				}
			}
		}
	} else {
		if len(ipAddrs) == 0 {
			return fmt.Errorf("nil ipAddrs inputed")
		}
		// TODO: support multiple IPs and IPv6
		ipAddr := ipAddrs[0]
		// ip slice is just in network endian
		ip := ipAddr.AsSlice()
		if len(ip) != 4 {
			return fmt.Errorf("invalid ip addr(%s), ipv4 is supported", ipAddr.String())
		}
		if remove {
			if ifindex != 0 {
				if err := r.detachTCForWorkload(ifindex); err != nil {
					multiErr = multierror.Append(multiErr, err)
				}
			} else {
				log.Debugf("zero ifindex for app removal")
			}
			if err := r.obj.AppInfo.Delete(ip); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
				multiErr = multierror.Append(multiErr, err)
			}
		} else {
			if err := r.attachTCForWorkLoad(ifindex); err != nil {
				multiErr = multierror.Append(multiErr, err)
				if err := r.detachTCForWorkload(ifindex); err != nil {
					multiErr = multierror.Append(multiErr, err)
				}
				return multiErr.ErrorOrNil()
			}
			if err := r.obj.AppInfo.Update(ip, mapInfo, ebpf.UpdateAny); err != nil {
				multiErr = multierror.Append(multiErr, err)
				if err := r.detachTCForWorkload(ifindex); err != nil {
					multiErr = multierror.Append(multiErr, err)
				}
			}
		}
	}
	return multiErr.ErrorOrNil()
}

func (r *RedirectServer) AcceptRequest(redirectArgs *RedirectArgs) {
	r.redirectArgsChan <- redirectArgs
}

func (r *RedirectServer) attachTCForZtunnel(ifindex, peerIndex uint32, namespace string) error {
	// attach to ztunnel host veth's ingress
	if err := r.attachTC(ifindex, IngressDir, r.ztunnelHostingressFd, r.ztunnelHostingressProgName); err != nil {
		return err
	}
	// attach to ztunnel veth's ingress in POD namespace
	if err := ns.WithNetNSPath(fmt.Sprintf("/var/run/netns/%s", namespace), func(ns.NetNS) error {
		if err := r.attachTC(peerIndex, IngressDir, r.ztunnelIngressFd, r.ztunnelIngressProgName); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (r *RedirectServer) detachTCForZtunnel(ifindex, peerIndex uint32, namespace string) error {
	if err := r.detachTC(ifindex, IngressDir, r.ztunnelHostingressProgName); err != nil {
		return fmt.Errorf("failed to detach TC ingress for ztunnel %d: %v", ifindex, err)
	}

	if err := r.delQdiscIfNeeded(ifindex); err != nil {
		return err
	}

	if err := ns.WithNetNSPath(fmt.Sprintf("/var/run/netns/%s", namespace), func(ns.NetNS) error {
		if err := r.detachTC(peerIndex, IngressDir, r.ztunnelIngressProgName); err != nil {
			return fmt.Errorf("failed to detach TC ingress for ztunnel %d(in pod ns): %v", peerIndex, err)
		}
		return r.delQdiscIfNeeded(peerIndex)
	}); err != nil {
		return err
	}
	return nil
}

func (r *RedirectServer) detachTCForWorkload(ifindex uint32) error {
	if err := r.detachTC(ifindex, IngressDir, r.outboundProgName); err != nil {
		return fmt.Errorf("failed to detach TC ingress for IfIndex %d: %v", ifindex, err)
	}
	if err := r.detachTC(ifindex, EgressDir, r.inboundProgName); err != nil {
		return fmt.Errorf("failed to detach TC egress for IfIndex %d: %v", ifindex, err)
	}

	return r.delQdiscIfNeeded(ifindex)
}

func (r *RedirectServer) attachTCForWorkLoad(ifindex uint32) error {
	// attach to workload host veth's egress
	if err := r.attachTC(ifindex, EgressDir, r.inboundFd, r.inboundProgName); err != nil {
		return err
	}
	// attach to workload host veth's ingress
	if err := r.attachTC(ifindex, IngressDir, r.outboundFd, r.outboundProgName); err != nil {
		return err
	}
	return nil
}

func (r *RedirectServer) attachTC(ifindex uint32, direction TCFilterDir, fd uint32, name string) error {
	rtnl, err := tc.Open(&tc.Config{})
	if err != nil {
		return err
	}
	defer func() {
		if err := rtnl.Close(); err != nil {
			log.Warnf("could not close rtnetlink socket: %v", err)
		}
	}()

	qdiscInfo := tc.Object{
		Msg: tc.Msg{
			Family:  unix.AF_UNSPEC,
			Ifindex: ifindex,
			Handle:  core.BuildHandle(tc.HandleRoot, 0x0000),
			Parent:  tc.HandleIngress,
		},
		Attribute: tc.Attribute{
			Kind: QdiscKind,
		},
	}
	// create qdisc on interface if not exists
	if err := rtnl.Qdisc().Add(&qdiscInfo); err != nil && !errors.Is(err, os.ErrExist) {
		log.Warnf("could not create %s qdisc to %d: %v", QdiscKind, ifindex, err)
		return err
	}
	flag := TcaBpfFlagActDiretct
	filter := tc.Object{
		Msg: tc.Msg{
			Family:  unix.AF_UNSPEC,
			Ifindex: ifindex,
			Handle:  1,
			// Info definition and usage could be referred from net/sched/cls_api.c 'tc_new_tfilter'
			// higher 16bits are used as priority, lower 16bits are used as protocol
			// refer include/net/sch_generic.h
			// prio is define as 'u32' while protocol is '__be16'. :(
			Info: core.BuildHandle(uint32(TcPrioFilter), uint32(htons(unix.ETH_P_ALL))),
		},
		Attribute: tc.Attribute{
			Kind: "bpf",
			BPF: &tc.Bpf{
				FD:    &fd,
				Name:  &name,
				Flags: &flag,
			},
		},
	}
	switch direction {
	case IngressDir:
		filter.Msg.Parent = core.BuildHandle(tc.HandleRoot, tc.HandleMinIngress)
	case EgressDir:
		filter.Msg.Parent = core.BuildHandle(tc.HandleRoot, tc.HandleMinEgress)
	}

	if err := rtnl.Filter().Add(&filter); err != nil && !errors.Is(err, os.ErrExist) {
		log.Warnf("could not attach %s eBPF: %v\n", direction, err)
		return err
	}
	return nil
}

func (r *RedirectServer) getTCFilters(ifindex uint32, direction TCFilterDir) ([]tc.Object, error) {
	rtnl, err := tc.Open(&tc.Config{})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rtnl.Close(); err != nil {
			log.Warnf("could not close rtnetlink socket: %v", err)
		}
	}()

	queryMsg := &tc.Msg{
		Family:  unix.AF_UNSPEC,
		Ifindex: ifindex,
	}
	switch direction {
	case IngressDir:
		queryMsg.Parent = core.BuildHandle(tc.HandleRoot, tc.HandleMinIngress)
	case EgressDir:
		queryMsg.Parent = core.BuildHandle(tc.HandleRoot, tc.HandleMinEgress)
	}
	return rtnl.Filter().Get(queryMsg)
}

func (r *RedirectServer) detachTC(ifindex uint32, direction TCFilterDir, name string) error {
	rtnl, err := tc.Open(&tc.Config{})
	if err != nil {
		return err
	}
	defer func() {
		if err := rtnl.Close(); err != nil {
			log.Warnf("could not close rtnetlink socket: %v", err)
		}
	}()

	objs, err := r.getTCFilters(ifindex, direction)
	if err != nil {
		return err
	}
	matchedIndices := []int{}
	for i, obj := range objs {
		if obj.Msg.Handle == 0 {
			continue
		}
		if obj.Attribute.Kind == "bpf" && obj.Attribute.BPF != nil &&
			obj.Attribute.BPF.Name != nil && *obj.Attribute.BPF.Name == name {
			matchedIndices = append(matchedIndices, i)
		}
	}
	if len(matchedIndices) == 0 {
		log.Debugf("No filter %s matched for ifindex(%d)", name, ifindex)
	}

	for _, idx := range matchedIndices {
		if err := rtnl.Filter().Delete(&objs[idx]); err != nil {
			log.Errorf("fail to delete filter: %v", err)
		}
	}
	return nil
}

func (r *RedirectServer) delQdiscIfNeeded(ifindex uint32) error {
	objs, err := r.getTCFilters(ifindex, IngressDir)
	if err != nil {
		return err
	}
	if len(objs) > 0 {
		log.Debugf("Ifindex %d still has other ingress filters, will not remove the clsact qdisc", ifindex)
		return nil
	}

	objs, err = r.getTCFilters(ifindex, EgressDir)
	if err != nil {
		return err
	}

	if len(objs) > 0 {
		log.Debugf("Ifindex %d still has other egress filters, will not remove the clsact qdisc", ifindex)
		return nil
	}

	// no filter exists, delete clsact qdisc
	return r.delClsactQdisc(ifindex)
}

func (r *RedirectServer) delClsactQdisc(ifindex uint32) error {
	rtnl, err := tc.Open(&tc.Config{})
	if err != nil {
		return err
	}
	defer func() {
		if err := rtnl.Close(); err != nil {
			log.Warnf("could not close rtnetlink socket: %v", err)
		}
	}()

	// delete clsact qdisc
	info := tc.Object{
		Msg: tc.Msg{
			Family:  unix.AF_UNSPEC,
			Ifindex: ifindex,
			Handle:  core.BuildHandle(tc.HandleRoot, 0x0000),
			Parent:  tc.HandleIngress,
		},
		Attribute: tc.Attribute{
			Kind: QdiscKind,
		},
	}
	err = rtnl.Qdisc().Delete(&info)
	if errors.Is(err, os.ErrNotExist) {
		log.Debugf("No qdisc configed for Ifindex: %d, %v", ifindex, err)
		return nil
	}

	return err
}

//nolint:unused
func (r *RedirectServer) dumpZtunnelInfo() (*mapInfo, error) {
	var info mapInfo
	if err := r.obj.ZtunnelInfo.Lookup(uint32(0), &info); err != nil {
		return nil, fmt.Errorf("failed to look up ztunnel info: %w", err)
	}
	return &info, nil
}

//nolint:unused
func (r *RedirectServer) dumpAppInfo() ([]uint32, []mapInfo) {
	var keyOut uint32
	var valueOut mapInfo
	var values []mapInfo
	var keys []uint32
	mapIter := r.obj.AppInfo.Iterate()
	for mapIter.Next(&keyOut, &valueOut) {
		keys = append(keys, keyOut)
		values = append(values, valueOut)

	}
	return keys, values
}

func htons(a uint16) uint16 {
	if isBigEndian {
		return a
	}
	return (a&0xff)<<8 | (a&0xff00)>>8
}

//nolint:unused
func htonl(a uint32) uint32 {
	if isBigEndian {
		return a
	}
	return (a&0xff)<<24 | (a&0xff00)<<8 | (a&0xff0000)>>8 | (a&0xff000000)>>24
}
