package cni

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MikeZappa87/kni-api/pkg/apis/runtime/beta"
	"github.com/MikeZappa87/libkni/pkg/netns"
	"github.com/containerd/go-cni"
	log "github.com/sirupsen/logrus"
)

type KNIConfig struct {
	IfPrefix string
	Db       string
	CNIBin   string
	CNIConf  string
}

type KNICNIService struct {
	c      cni.CNI
	store  *Store
	config KNIConfig
}

func CreateDefaultConfig() KNIConfig {
	return KNIConfig{
		IfPrefix: "eth",
		Db:       "net.db",
		CNIBin:   "/opt/cni/bin",
		CNIConf:  "/etc/cni/net.d",
	}
}

func NewKniService(config *KNIConfig) (beta.KNIServer, error) {
	log.Info("starting kni network runtime service !bang")

	opts := []cni.Opt{
		cni.WithLoNetwork,
		cni.WithAllConf}

	initopts := []cni.Opt{
		cni.WithMinNetworkCount(2),
		cni.WithInterfacePrefix(config.IfPrefix),
	}

	cni, err := cni.New(initopts...)

	if err != nil {
		return nil, err
	}

	db, err := New(config.Db)

	if err != nil {
		return nil, err
	}

	kni := &KNICNIService{
		c:      cni,
		store:  db,
		config: *config,
	}

	log.Info("loading cni with config: +v", config.CNIConf)

	sync, err := newCNINetConfSyncer(config.CNIConf, cni, opts)

	if err != nil {
		return nil, err
	}

	go func() {
		sync.syncLoop()
	}()

	log.Info("cni has been loaded")

	return kni, nil
}

func (k *KNICNIService) CreateNetwork(ctx context.Context, req *beta.CreateNetworkRequest) (*beta.CreateNetworkResponse, error) {
	ns, err := netns.NewNetNS("/run/netns", fmt.Sprintf("kni-%s-%s", req.Namespace, req.Name))

	if err != nil {
		log.Errorf("unable to create netns: %s name: %s namespace: %s", err.Error(), req.Name, req.Namespace)
		return nil, err
	}

	log.Infof("created netns name: %s namespace: %s", req.Name, req.Namespace)

	return &beta.CreateNetworkResponse{
		NetnsPath: ns.GetPath(),
	}, nil
}

func (k *KNICNIService) DeleteNetwork(ctx context.Context, req *beta.DeleteNetworkRequest) (*beta.DeleteNetworkResponse, error) {
	netnspath := ""

	if req.Id != "" {
		data, err := k.store.Query(req.Id)

		if err != nil {
			log.Errorf("unable retrieve sandbox information for %s", req.Id)
			return nil, err
		}

		netnspath = data.Netns_path
	} else {
		netnspath = fmt.Sprintf("/run/netns/kni-%s-%s", req.Namespace, req.Name)
	}

	log.Infof("deleting netns: %s", netnspath)

	ns := netns.LoadNetNS(netnspath)

	if _, err := ns.Closed(); err != nil {
		return nil, err
	}

	if err := ns.Remove(); err != nil {
		log.Errorf("unable to delete netns: %s name: %s namespace: %s", err.Error(), req.Name, req.Namespace)
		return nil, err
	}

	if err := k.store.Delete(req.Id); err != nil {
		log.Errorf("unable to delete record id: %s %v", req.Id, err)

		return nil, err
	}

	return &beta.DeleteNetworkResponse{}, nil
}

func (k *KNICNIService) AttachInterface(ctx context.Context, req *beta.AttachInterfaceRequest) (*beta.AttachInterfaceResponse, error) {
	log.Infof("attach rpc request for id %s", req.Id)

	opts, err := cniNamespaceOpts(req.Id, req.Name, req.Namespace, "", req.Labels,
		req.Annotations, req.Extradata, req.PortMappings, req.DnsConfig)

	if err != nil {
		return nil, err
	}

	if _, ok := req.Extradata["netns"]; !ok {
		return nil, fmt.Errorf("pod annotation id: %s has no netns", req.Id)
	}

	netns := req.Extradata["netns"]

	var res *cni.Result

	res, err = k.c.SetupSerially(ctx, req.Id, netns, opts...)

	if err != nil {
		log.Errorf("unable to execute CNI ADD: %s", err.Error())

		return nil, err
	}

	cniResult, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}

	log.Infof("CNI Result: %s", string(cniResult))

	store := NetworkStorage{
		IP:           make(map[string]*beta.IPConfig),
		Annotations:  req.Annotations,
		Extradata:    req.Extradata,
		PodName:      req.Name,
		PodNamespace: req.Namespace,
		Netns_path:   netns,
	}

	for outk, outv := range res.Interfaces {
		data := &beta.IPConfig{}
		store.IP[outk] = data
		data.Mac = outv.Mac

		for _, v := range outv.IPConfigs {
			data.Ip = append(data.Ip, v.IP.String())
		}
	}

	log.WithField("ipconfigs", store.IP).Info("cni add executed")

	err = k.store.Save(req.Id, store)

	if err != nil {
		log.Errorf("unable to save record for id: %s: %s", req.Id, err.Error())
		return nil, err
	}

	return &beta.AttachInterfaceResponse{
		Ipconfigs: store.IP,
	}, nil
}

func (k *KNICNIService) DetachInterface(ctx context.Context, req *beta.DetachInterfaceRequest) (*beta.DetachInterfaceResponse, error) {
	log.Infof("detach rpc request for id %s", req.Id)

	opts := []cni.NamespaceOpts{
		cni.WithArgs("IgnoreUnknown", "1"),
		cni.WithLabels(req.Labels),
		cni.WithLabels(req.Annotations),
		cni.WithLabels(req.Extradata),
	}

	query, err := k.store.Query(req.Id)

	if err != nil {
		log.Errorf("unable to query record id: %s %v", req.Id, err)

		return nil, err
	}

	if cgroup := query.Extradata["cgroupPath"]; cgroup != "" {
		opts = append(opts, cni.WithCapabilityCgroupPath(cgroup))
		log.Infof("cgroup: %s", cgroup)
	}

	netns := query.Extradata["netns"]

	err = k.c.Remove(ctx, req.Id, netns, opts...)

	if err != nil {
		log.Errorf("unable to execute CNI DEL: %s", err.Error())

		return nil, err
	}

	return &beta.DetachInterfaceResponse{}, nil
}

func (k *KNICNIService) SetupNodeNetwork(context.Context, *beta.SetupNodeNetworkRequest) (*beta.SetupNodeNetworkResponse, error) {
	//Setup the initial node network

	return nil, nil
}

func (k *KNICNIService) QueryPodNetwork(ctx context.Context, req *beta.QueryPodNetworkRequest) (*beta.QueryPodNetworkResponse, error) {

	log.Infof("query pod rpc request id: %s", req.Id)

	data, err := k.store.Query(req.Id)

	if data.IP == nil {
		return &beta.QueryPodNetworkResponse{}, nil
	}

	log.Infof("ipconfigs received for id: %s ip: %s", req.Id, data.IP)

	if err != nil {
		return nil, err
	}

	return &beta.QueryPodNetworkResponse{
		Ipconfigs: data.IP,
	}, nil
}

// !bang
func (k *KNICNIService) QueryNodeNetworks(ctx context.Context, req *beta.QueryNodeNetworksRequest) (*beta.QueryNodeNetworksResponse, error) {
	networks := []*beta.Network{}

	if err := k.c.Status(); err != nil {
		networks = append(networks, &beta.Network{
			Name:      "default",
			Ready:     false,
			Extradata: map[string]string{},
		})
	} else {
		networks = append(networks, &beta.Network{
			Name:  "default",
			Ready: true,
		})
	}

	return &beta.QueryNodeNetworksResponse{
		Networks: networks,
	}, nil
}
