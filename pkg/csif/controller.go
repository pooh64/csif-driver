package csif

import (
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type volAccessType int

const (
	volAccessMount volAccessType = iota
	volAccessBlock
)

type csifControllerServer struct {
	cd      *csifDriver
	volumes map[string]*csifVolume
}

func newCsifControllerServer(driver *csifDriver) *csifControllerServer {
	return &csifControllerServer{
		cd:      driver,
		volumes: map[string]*csifVolume{},
	}
}

// ControllerServer related info
type csifVolume struct {
	Name       string
	ID         string
	Size       int64
	AccessType volAccessType
	Disk       *csifDisk
}

func (cs *csifControllerServer) getVolumeByID(volID string) (*csifVolume, error) {
	if vol, ok := cs.volumes[volID]; ok {
		return vol, nil
	}
	return nil, fmt.Errorf("no volID=%s in volumes", volID)
}

func (cs *csifControllerServer) getVolumeByName(volName string) (*csifVolume, error) {
	for _, vol := range cs.volumes {
		if vol.Name == volName {
			return vol, nil
		}
	}
	return nil, fmt.Errorf("no volName=%s in volumes", volName)
}

func (cs *csifControllerServer) createVolume(req *csi.CreateVolumeRequest, accessType volAccessType) (*csifVolume, error) {
	name := req.GetName()
	glog.V(4).Infof("creating csif volume: %s", name)

	volID, err := newUUID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate uuid: %w", err)
	}

	switch accessType {
	case volAccessMount, volAccessBlock:
	default:
		return nil, fmt.Errorf("wrong access type %v", accessType)
	}

	disk := newCsifDisk(cs.cd)

	if err := disk.Create(req, volID); err != nil {
		return nil, fmt.Errorf("disk.Create failed: %v", err)
	}

	vol := &csifVolume{
		Name:       name,
		ID:         volID,
		Size:       req.CapacityRange.GetRequiredBytes(),
		AccessType: accessType,
		Disk:       disk,
	}
	cs.volumes[volID] = vol
	return vol, nil
}

func (cs *csifControllerServer) deleteVolume(volID string) error {
	glog.V(4).Infof("deleting csif volume: %s", volID)

	vol, err := cs.getVolumeByID(volID)
	if err != nil {
		glog.V(5).Infof("deleting nonexistent volume")
		return nil
	}

	if err := vol.Disk.Destroy(); err != nil {
		return fmt.Errorf("failed to disconnect disk: %v", err)
	}

	delete(cs.volumes, volID)
	return nil
}

func (cs *csifControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: cs.getCSCapabilities(),
	}, nil
}

func (cs *csifControllerServer) getCSCapabilities() []*csi.ControllerServiceCapability {
	rpcCap := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT, // TODO: NI, remove
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,          // TODO: NI, remove
		//csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
	}
	var csCap []*csi.ControllerServiceCapability

	for _, cap := range rpcCap {
		csCap = append(csCap, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		})
	}
	return csCap
}

func (cs *csifControllerServer) validateCSCapability(c csi.ControllerServiceCapability_RPC_Type) error {
	if c == csi.ControllerServiceCapability_RPC_UNKNOWN {
		return nil
	}

	for _, cap := range cs.getCSCapabilities() {
		if c == cap.GetRpc().GetType() {
			return nil
		}
	}
	return status.Errorf(codes.InvalidArgument, "CSCapability unsupported: %s", c)
}

func obtainVolumeCapabilitiy(caps []*csi.VolumeCapability) (volAccessType, error) {
	isMount, isBlock := false, false

	for _, cap := range caps {
		if cap.GetMount() != nil {
			isMount = true
		}
		if cap.GetBlock() != nil {
			isBlock = true
		}
	}

	if isMount && isBlock {
		return volAccessMount, status.Error(codes.InvalidArgument, "block+mount access type")
	}

	if isBlock {
		return volAccessBlock, nil
	}
	return volAccessMount, nil
}

func (cs *csifControllerServer) csifVolumeToCSI(vol *csifVolume, topo []*csi.Topology) *csi.Volume {
	attr := vol.Disk.SaveContext()

	return &csi.Volume{
		VolumeId:           vol.ID,
		CapacityBytes:      int64(vol.Size),
		AccessibleTopology: topo,
		VolumeContext:      attr,
	}
}

func (cs *csifControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (resp *csi.CreateVolumeResponse, finalErr error) {
	if err := cs.validateCSCapability(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid request: %v", req)
		return nil, err
	}

	if len(req.GetName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "No volName in request")
	}

	caps := req.GetVolumeCapabilities()
	if caps == nil {
		return nil, status.Error(codes.InvalidArgument, "nil vol.caps")
	}

	accessType, err := obtainVolumeCapabilitiy(caps)
	if err != nil {
		return nil, err
	}

	capacity := int64(req.GetCapacityRange().GetRequiredBytes())

	// TODO: load and set tology restrictions properly
	// note: identity.go: VOLUME_ACCESSIBILITY_CONSTRAINTS
	//nodeTopo := csi.Topology{Segments: map[string]string{TopologyKeyNode: cs.cd.nodeID}}
	//topologies := []*csi.Topology{&nodeTopo}

	if req.GetVolumeContentSource() != nil {
		return nil, status.Error(codes.InvalidArgument, "VolumeContentSource feautures unsupported")
	}

	// If volume exists - verify parameters, respond
	if vol, err := cs.getVolumeByName(req.GetName()); err == nil {
		glog.V(4).Infof("%s volume exists, veifying parameters", req.GetName())
		if vol.Size != capacity {
			return nil, status.Errorf(codes.AlreadyExists, "vol.size mismatch")
		}

		return &csi.CreateVolumeResponse{
			Volume: cs.csifVolumeToCSI(vol, nil),
		}, nil
	}

	vol, err := cs.createVolume(req, accessType)
	if err != nil {
		return nil, fmt.Errorf("failed to create volume %v: %w", req.GetName(), err)
	}
	glog.V(4).Infof("volume: %s created", vol.ID)

	return &csi.CreateVolumeResponse{
		Volume: cs.csifVolumeToCSI(vol, nil),
	}, nil
}

func (cs *csifControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if err := cs.validateCSCapability(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid request: %v", req)
		return nil, err
	}

	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "No volID in request")
	}

	volId := req.GetVolumeId()
	if err := cs.deleteVolume(volId); err != nil {
		return nil, fmt.Errorf("deleteVolume %v failed: %w", volId, err)
	}
	glog.V(4).Infof("volume %v deleted", volId)

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *csifControllerServer) ControllerPublishVolume(_ context.Context, _ *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func (cs *csifControllerServer) ControllerUnpublishVolume(_ context.Context, _ *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func (cs *csifControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func (cs *csifControllerServer) ListVolumes(_ context.Context, _ *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func (cs *csifControllerServer) GetCapacity(_ context.Context, _ *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func (cs *csifControllerServer) CreateSnapshot(_ context.Context, _ *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "snapshots are unimplemented")
}

func (cs *csifControllerServer) DeleteSnapshot(_ context.Context, _ *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "snapshots are unimplemented")
}

func (cs *csifControllerServer) ListSnapshots(_ context.Context, _ *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "snapshots are unimplemented")
}

func (cs *csifControllerServer) ControllerExpandVolume(_ context.Context, _ *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func (cs *csifControllerServer) ControllerGetVolume(_ context.Context, _ *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}
