// Copyright 2019 NetApp, Inc. All Rights Reserved.

// Package sdk provides a high-level interface to the Azure NetApp Files SDK
package sdk

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/Azure/azure-sdk-for-go/profiles/2019-03-01/resources/mgmt/resources"
	"github.com/cenkalti/backoff/v4"

	// Forced to use "latest" in order to get subnet Delegations
	"github.com/Azure/azure-sdk-for-go/profiles/latest/network/mgmt/network"
	"github.com/Azure/azure-sdk-for-go/services/netapp/mgmt/2019-07-01/netapp"
	"github.com/Azure/go-autorest/autorest/azure"
	azauth "github.com/Azure/go-autorest/autorest/azure/auth"
	log "github.com/sirupsen/logrus"

	"github.com/netapp/trident/storage"
)

const (
	subnetTemplate      = "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/virtualNetworks/%s/subnets/%s"
	VolumeCreateTimeout = 10 * time.Second
	// The CSI Snapshot sidecar has a timeout of 5 minutes.  We need to come in under that in order
	// to avoid bigger problems.
	SnapshotTimeout = 240 * time.Second
	DefaultTimeout  = 120 * time.Second
	tridentLabelTag = "trident"
)

// ClientConfig holds configuration data for the API driver object.
type ClientConfig struct {

	// Azure API authentication parameters
	SubscriptionID string
	TenantID       string
	ClientID       string
	ClientSecret   string

	// Options
	DebugTraceFlags map[string]bool
}

// AzureClient holds operational Azure SDK objects
type AzureClient struct {
	Ctx context.Context
	netapp.AccountsClient
	netapp.PoolsClient
	netapp.VolumesClient
	netapp.MountTargetsClient
	netapp.SnapshotsClient
	AuthConfig            azauth.ClientCredentialsConfig
	ResourcesClient       resources.Client
	VirtualNetworksClient network.VirtualNetworksClient
	SubnetsClient         network.SubnetsClient
	AzureResources
}

// Client encapsulates connection details
type Client struct {
	terminated bool
	config     *ClientConfig
	SDKClient  *AzureClient
}

// NewSDKClient allocates the various clients for the SDK
func NewSDKClient(config *ClientConfig) (c *AzureClient) {
	c = new(AzureClient)
	c.Ctx = context.Background()

	c.AuthConfig = azauth.NewClientCredentialsConfig(config.ClientID, config.ClientSecret, config.TenantID)
	c.AuthConfig.AADEndpoint = azure.PublicCloud.ActiveDirectoryEndpoint
	c.AuthConfig.Resource = azure.PublicCloud.ResourceManagerEndpoint

	c.AccountsClient = netapp.NewAccountsClient(config.SubscriptionID)
	c.PoolsClient = netapp.NewPoolsClient(config.SubscriptionID)
	c.VolumesClient = netapp.NewVolumesClient(config.SubscriptionID)
	c.MountTargetsClient = netapp.NewMountTargetsClient(config.SubscriptionID)
	c.SnapshotsClient = netapp.NewSnapshotsClient(config.SubscriptionID)
	c.ResourcesClient = resources.NewClient(config.SubscriptionID)
	c.VirtualNetworksClient = network.NewVirtualNetworksClient(config.SubscriptionID)
	c.SubnetsClient = network.NewSubnetsClient(config.SubscriptionID)
	return
}

// Authenticate plumbs the authorization through to subclients
func (c *AzureClient) Authenticate() (err error) {
	c.AccountsClient.Authorizer, err = c.AuthConfig.Authorizer()
	c.PoolsClient.Authorizer, err = c.AuthConfig.Authorizer()
	c.VolumesClient.Authorizer, err = c.AuthConfig.Authorizer()
	c.MountTargetsClient.Authorizer, err = c.AuthConfig.Authorizer()
	c.SnapshotsClient.Authorizer, err = c.AuthConfig.Authorizer()
	c.ResourcesClient.Authorizer, err = c.AuthConfig.Authorizer()
	c.VirtualNetworksClient.Authorizer, err = c.AuthConfig.Authorizer()
	c.SubnetsClient.Authorizer, err = c.AuthConfig.Authorizer()
	return
}

// NewDriver is a factory method for creating a new instance.
func NewDriver(config ClientConfig) *Client {

	c := NewSDKClient(&config)
	d := &Client{
		config:    &config,
		SDKClient: c,
	}

	return d
}

// Init runs startup logic after allocating the driver resources
func (d *Client) Init(pools map[string]*storage.Pool) (err error) {

	// Find out what we have to work with in Azure
	d.discoveryInit()

	// Map vpools to backend
	for _, p := range pools {
		d.RegisterStoragePool(*p)
	}

	return nil
}

// Terminate signals any running threads to exit at their earliest convenience
func (d *Client) Terminate() {
	d.terminated = true
}

func internalShortname(full string) string {
	var re = regexp.MustCompile("^(.+[^/])+/")
	shortname := re.ReplaceAllString(full, "")
	return shortname
}

// poolShortname returns the trailing component of a full poolname, which is needed in
// most situations where a poolname needs to be specified.  The full name produces an
// error.
func poolShortname(full string) string {
	return internalShortname(full)
}

// volumeShortname returns the trailing component of a full volume name, which is needed in
// most situations where a volume name needs to be specified.  The full name fails to match
// what we actually call it.
func volumeShortname(full string) string {
	return internalShortname(full)
}

// serviceLevelFromString converts a string into a predefined service level
func serviceLevelFromString(level string) netapp.ServiceLevel {
	return netapp.ServiceLevel(level)
}

// exportPolicyExportOne converts one export rule at a time on behalf of exportPolicyExport
func exportPolicyExportOne(er *ExportRule) *netapp.ExportPolicyRule {
	var naep netapp.ExportPolicyRule

	cnv := int32(er.RuleIndex)
	naep.RuleIndex = &cnv

	naep.UnixReadOnly = &er.UnixReadOnly
	naep.UnixReadWrite = &er.UnixReadWrite
	naep.Cifs = &er.Cifs
	naep.Nfsv3 = &er.Nfsv3
	naep.Nfsv41 = &er.Nfsv41
	naep.AllowedClients = &er.AllowedClients

	return &naep
}

// exportPolicyExport turns an internal ExportPolicy into something consumable by the SDK
func exportPolicyExport(ep *ExportPolicy) *netapp.VolumePropertiesExportPolicy {
	var navp = netapp.VolumePropertiesExportPolicy{}
	var rules = []netapp.ExportPolicyRule{}

	for _, rule := range ep.Rules {
		var naep = exportPolicyExportOne(&rule)
		rules = append(rules, *naep)
	}
	navp.Rules = &rules

	return &navp
}

// exportPolicyImportOne converts one export rule at a time on behalf of exportPolicyImport
func exportPolicyImportOne(epr *netapp.ExportPolicyRule) *ExportRule {
	var naer ExportRule

	// These attributes are not always fully populated, so hide them inside nil-checks
	if epr.RuleIndex != nil {
		naer.RuleIndex = int(*epr.RuleIndex)
	}

	if epr.UnixReadOnly != nil {
		naer.UnixReadOnly = *epr.UnixReadOnly
	}

	if epr.UnixReadWrite != nil {
		naer.UnixReadWrite = *epr.UnixReadWrite
	}

	if epr.Cifs != nil {
		naer.Cifs = *epr.Cifs
	}

	if epr.Nfsv3 != nil {
		naer.Nfsv3 = *epr.Nfsv3
	}

	if epr.Nfsv41 != nil {
		naer.Nfsv41 = *epr.Nfsv41
	}

	if epr.AllowedClients != nil {
		naer.AllowedClients = *epr.AllowedClients
	}

	return &naer
}

// exportPolicyImport turns an SDK ExportPolicy into an internal one
func exportPolicyImport(ep *netapp.VolumePropertiesExportPolicy) *ExportPolicy {
	var naeps = ExportPolicy{}
	var rules = []ExportRule{}

	for _, rule := range *ep.Rules {
		var naep = exportPolicyImportOne(&rule)
		rules = append(rules, *naep)
	}
	naeps.Rules = rules

	return &naeps
}

// newFileSystemFromVolume creates a new internal FileSystem struct from a netapp.Volume
// as best it can.  There are fields in FileSystem that do not exist in netapp.Volume, and
// vice-versa.
func (d *Client) newFileSystemFromVolume(vol *netapp.Volume, cookie *AzureCapacityPoolCookie) (*FileSystem, error) {

	pool, err := d.getCapacityPool(*cookie.CapacityPoolName)
	if err != nil {
		return nil, err
	}

	fs := FileSystem{
		Name:             *vol.Name,
		Location:         *vol.Location,
		CapacityPoolName: pool.Name,
		ServiceLevel:     pool.ServiceLevel,
	}

	// VolumeProperties strings are not always populated, nor is 'ID'
	if vol.ID != nil {
		fs.ID = *vol.ID
	}

	if vol.VolumeProperties.FileSystemID != nil {
		fs.FileSystemID = *vol.VolumeProperties.FileSystemID
	}

	if vol.VolumeProperties.CreationToken != nil {
		fs.CreationToken = *vol.VolumeProperties.CreationToken
	}

	if vol.VolumeProperties.ProvisioningState != nil {
		fs.ProvisioningState = *vol.VolumeProperties.ProvisioningState
	}

	if vol.VolumeProperties.UsageThreshold != nil {
		fs.QuotaInBytes = *vol.VolumeProperties.UsageThreshold
	}

	if vol.VolumeProperties.SubnetID != nil {
		fs.Subnet = *vol.VolumeProperties.SubnetID
	}

	if vol.VolumeProperties.ExportPolicy != nil {
		fs.ExportPolicy = *exportPolicyImport(vol.VolumeProperties.ExportPolicy)
	}

	return &fs, nil
}

// getVolumesFromPool gets a set of volumes belonging to a single capacity pool
func (d *Client) getVolumesFromPool(cookie *AzureCapacityPoolCookie, poolName string) (*[]FileSystem, error) {

	// poolName is an override, use the cookie's if not specified
	if poolName == "" {
		poolName = *cookie.CapacityPoolName
	}

	volumelist, err := d.SDKClient.VolumesClient.List(d.SDKClient.Ctx,
		*cookie.ResourceGroup, *cookie.NetAppAccount, poolName)
	if err != nil {
		log.Errorf("Error fetching volumes from pool %s: %s", poolName, err)
		return nil, err
	}

	volumes := *volumelist.Value

	var fses []FileSystem

	for _, v := range volumes {
		fs, err := d.newFileSystemFromVolume(&v, cookie)
		if err != nil {
			log.Errorf("Internal error creating filesystem")
			return nil, err
		}
		fses = append(fses, *fs)
	}

	return &fses, nil
}

// GetVolumes returns a list of ALL volumes
func (d *Client) GetVolumes() (*[]FileSystem, error) {
	var filesystems []FileSystem

	pools := d.getCapacityPools()

	for _, p := range *pools {
		shortname := poolShortname(p.Name)
		cookie, err := d.GetCookieByCapacityPoolName(shortname)
		if err != nil {
			return nil, err
		}

		fs, err := d.getVolumesFromPool(cookie, poolShortname(p.Name))
		if err != nil {
			log.Errorf("Error fetching volumes from pool %s: %s", p.Name, err)
			return nil, err
		}
		for _, f := range *fs {
			// Strip off the full name as soon as we see it.
			f.Name = volumeShortname(f.Name)
			filesystems = append(filesystems, f)
		}
	}

	return &filesystems, nil
}

// GetVolumeByName fetches a Filesystem based on its readable name
func (d *Client) GetVolumeByName(name string) (*FileSystem, error) {
	// See GetVolumeByCreationToken for comments on Azure API searchability.
	//
	// Even lacking that capability, this could be a direct 'Get' call -- bypassing
	// the bogus search -- if we cached volume information along with the discovered
	// Azure resources.  Currently the cache contains only relatively static, read-only
	// information.  Volumes change much more rapidly and through different paths.
	// If this volume-fetching becomes a performance bottleneck, maintaining a cache
	// here could be very helpful.

	filesystems, err := d.GetVolumes()
	if err != nil {
		return nil, err
	}

	for _, f := range *filesystems {
		if f.Name == name {
			return &f, nil
		}
	}

	return nil, fmt.Errorf("filesystem '%s' not found", name)
}

// GetVolumeByCreationToken fetches a Filesystem by its immutable creation token
func (d *Client) GetVolumeByCreationToken(creationToken string) (*FileSystem, error) {
	// SDK does not support searching by creation token. Or anything besides pool+name,
	// for that matter. Get all volumes and find it ourselves. This is far from ideal.
	filesystems, err := d.GetVolumes()
	if err != nil {
		return nil, err
	}

	for _, f := range *filesystems {
		if f.CreationToken == creationToken {
			return &f, nil
		}
	}

	return nil, fmt.Errorf("filesystem with token '%s' not found", creationToken)
}

// VolumeExistsByCreationToken checks whether a volume exists using its token as a key
func (d *Client) VolumeExistsByCreationToken(creationToken string) (bool, *FileSystem, error) {
	fs, _ := d.GetVolumeByCreationToken(creationToken)

	// Volume exists
	if fs != nil {
		return true, fs, nil
	}

	return false, nil, nil
}

// GetVolumeByID returns a Filesystem based on its ID
func (d *Client) GetVolumeByID(fileSystemID string) (*FileSystem, error) {
	// Same problem
	filesystems, err := d.GetVolumes()
	if err != nil {
		return nil, err
	}

	for _, f := range *filesystems {
		if f.FileSystemID == fileSystemID {
			return &f, nil
		}
	}

	return nil, fmt.Errorf("filesystem with id '%s' not found", fileSystemID)
}

// WaitForVolumeState watches for a desired volume state and returns when that state is achieved
func (d *Client) WaitForVolumeState(
	filesystem *FileSystem, desiredState string, abortStates []string, maxElapsedTime time.Duration,
) (string, error) {
	volumeState := ""

	checkVolumeState := func() error {
		var f *FileSystem
		var err error

		// Properties fields are not populated on create; use Name instead
		if filesystem.FileSystemID == "" {
			f, err = d.GetVolumeByName(filesystem.Name)
		} else {
			f, err = d.GetVolumeByID(filesystem.FileSystemID)
		}
		if err != nil {
			volumeState = ""
			// This is a bit of a hack, but there is no 'Deleted' state in Azure -- the
			// volume just vanishes.  If we failed to query the volume info and we're trying
			// to transition to StateDeleted, try a raw fetch of the volume and if we
			// get back a 404, then call it a day.  Otherwise, log the error as usual.
			if desiredState == StateDeleted {
				cookie, err := d.GetCookieByCapacityPoolName(filesystem.CapacityPoolName)
				if err != nil {
					return fmt.Errorf("waitForVolumeState internal error re-checking volume: %v", err)
				}

				vol, err := d.SDKClient.VolumesClient.Get(d.SDKClient.Ctx, *cookie.ResourceGroup,
					*cookie.NetAppAccount, *cookie.CapacityPoolName, filesystem.Name)
				if err != nil && vol.StatusCode == 404 {
					// Deleted!
					log.Debugf("Implied deletion for volume %s", filesystem.Name)
					return nil
				} else {
					return fmt.Errorf("waitForVolumeState internal error re-checking volume: %v", err)
				}
			}

			return fmt.Errorf("could not get volume status; %v", err)
		}
		volumeState = f.ProvisioningState

		if volumeState == desiredState {
			return nil
		}

		err = fmt.Errorf("volume state is %s, not %s", volumeState, desiredState)

		// Return a permanent error to stop retrying if we reached one of the abort states
		for _, abortState := range abortStates {
			if volumeState == abortState {
				return backoff.Permanent(TerminalState(err))
			}
		}

		return err
	}
	stateNotify := func(err error, duration time.Duration) {
		log.WithFields(log.Fields{
			"increment": duration,
			"message":   err.Error(),
		}).Debugf("Waiting for volume state.")
	}
	stateBackoff := backoff.NewExponentialBackOff()
	stateBackoff.MaxElapsedTime = maxElapsedTime
	stateBackoff.MaxInterval = 5 * time.Second
	stateBackoff.RandomizationFactor = 0.1
	stateBackoff.InitialInterval = backoff.DefaultInitialInterval
	stateBackoff.Multiplier = 1.414

	log.WithField("desiredState", desiredState).Info("Waiting for volume state.")

	if err := backoff.RetryNotify(checkVolumeState, stateBackoff, stateNotify); err != nil {
		if terminalStateErr, ok := err.(*TerminalStateError); ok {
			log.Errorf("Volume reached terminal state: %v.", terminalStateErr)
		} else {
			log.Errorf("Volume state was not %s after %3.2f seconds.",
				desiredState, stateBackoff.MaxElapsedTime.Seconds())
		}
		return volumeState, err
	}

	log.WithField("desiredState", desiredState).Debug("Desired volume state reached.")

	return volumeState, nil
}

// CreateVolume creates a new volume
func (d *Client) CreateVolume(request *FilesystemCreateRequest) (*FileSystem, error) {

	// Fetch the required cached access details from the pool name.  Hack alert: If the
	// pool name is DoNotUseSPoolName, then we are coming from a path where this information
	// was not available, but in that case the CapacityPool will have been filled in instead.
	cookie := &AzureCapacityPoolCookie{}
	err := errors.New("")
	if request.PoolID == DoNotUseSPoolName {
		cookie, err = d.GetCookieByCapacityPoolName(request.CapacityPool)
	} else {
		cookie, err = d.GetCookieByStoragePoolName(request.PoolID)
	}
	if err != nil {
		return nil, err
	}

	resourceGroup := *cookie.ResourceGroup
	netAppAccount := *cookie.NetAppAccount
	cpoolName := *cookie.CapacityPoolName
	vNet := request.VirtualNetwork
	subnet := request.Subnet

	// We should only be seeing the trident telemetry label here.  Use a tag to store it.
	tags := make(map[string]string)
	if len(request.Labels) > 1 {
		log.Errorf("Too many labels (%d) in volume create request.", len(request.Labels))
	}
	for _, val := range request.Labels {
		tags[tridentLabelTag] = val
	}

	// Get the capacity pool so we can validate location and inherit service level
	cpool, err := d.SDKClient.PoolsClient.Get(d.SDKClient.Ctx, resourceGroup, netAppAccount, cpoolName)
	if err != nil {
		return nil, fmt.Errorf("couldn't get capacity pool: %v", err)
	}

	// Fetch a volume first.  This has probably already been checked once but we
	// need the sdk structs.
	newVol, err := d.SDKClient.VolumesClient.Get(d.SDKClient.Ctx, resourceGroup, netAppAccount, cpoolName, request.Name)
	if err != nil && newVol.StatusCode != 404 {
		return nil, fmt.Errorf("couldn't get volume: %v", err)
	}
	if err == nil {
		return nil, errors.New("volume already exists")
	}

	// Location is required, and is also not something you can deviate from wrt cpools
	location := request.Location
	if location == "" {
		location = *cpool.Location
	} else if location != *cpool.Location {
		return nil, errors.New("new volume requested with location different from capacity pool location")
	}

	newVol.Location = &location
	newVol.Name = &request.Name
	newVol.Tags = tags
	newVol.VolumeProperties = &netapp.VolumeProperties{
		CreationToken:  &request.CreationToken,
		ServiceLevel:   cpool.ServiceLevel,
		UsageThreshold: &request.QuotaInBytes,
		ExportPolicy:   exportPolicyExport(&request.ExportPolicy),
		ProtocolTypes:  &request.ProtocolTypes,
	}

	// Figure out what we need to do about vnets and subnets.  The basic plan for a normal
	// volume create is:
	//
	//   - If subnet and vnet are both specified, trust the values and use them directly.
	//   - If subnet is specified and vnet isn't, resolve the subnet and derive the vnet
	//   - If subnet is not specified and vnet is, do a random selection just within that vnet
	//   - If subnet and vnet are both unspecified, do a random selection by location across all vnets
	//
	// Clone creation is different: the subnet is always specified and it is in a different, fully
	// qualified format, so (below) we ignore all this.  This is controlled by snapshotID being
	// non-nil.

	subnetID := subnet

	if request.SnapshotID != "" {
		// Clone case; 'subnetID' is already kosher. Go ahead and populate the snapshot ID here
		// as well since we've just checked it. We only populate this newVol field if actually
		// using it for a clone create; an empty string will generate an Azure API error.
		newVol.SnapshotID = &request.SnapshotID
	} else {
		// New volume case, we have some work to do.  Note that we need to find out the vNet's
		// resource group in all cases here - it may differ from the Capacity Pool's.
		vnetRG := ""
		if subnet == "" {
			logstr := "No subnet specified in volume creation request, selecting "
			if vNet != "" {
				log.Debugf(logstr+"from vnet %s.", vNet)
			} else {
				log.Debugf(logstr+"at random in %s.", *cpool.Location)
			}
			randomSubnet := d.randomSubnetForLocation(vNet, *cpool.Location)
			if randomSubnet == nil {
				return nil, fmt.Errorf("could not find a suitable subnet in %s", *cpool.Location)
			}
			subnet = randomSubnet.Name
			vNet = randomSubnet.VirtualNetwork.Name
			vnetRG = randomSubnet.VirtualNetwork.ResourceGroup
		} else if vNet == "" {
			derivedVNet, err := d.virtualNetworkForSubnet(subnet, *cpool.Location)
			if err != nil {
				return nil, fmt.Errorf("create volume couldn't derive virtual network: %v", err)
			}
			vNet = derivedVNet.Name
			vnetRG = derivedVNet.ResourceGroup
		}

		if vnetRG == "" {
			vnetRGp, err := d.resourceGroupForVirtualNetwork(vNet)
			if err != nil {
				return nil, fmt.Errorf("create volume couldn't map virtual network %v to a resource group", err)
			}
			vnetRG = *vnetRGp
		}

		// Reformat the subnetID into the necessary structured parameter list. This specification
		// is pretty gross: it's the only place I know of where we need to know anything about
		// azure-internal details like this subnet template.  Bit of a rough patch in the API.
		subnetID = fmt.Sprintf(subnetTemplate, d.config.SubscriptionID, vnetRG, vNet, subnet)
	}

	newVol.SubnetID = &subnetID

	if d.config.DebugTraceFlags["api"] {
		log.WithFields(log.Fields{
			"name":           request.Name,
			"creationToken":  request.CreationToken,
			"resourceGroup":  resourceGroup,
			"netAppAccount":  netAppAccount,
			"capacityPool":   cpoolName,
			"virtualNetwork": vNet,
			"subnet":         subnet,
			"snapshotID":     request.SnapshotID,
		}).Debug("Issuing create request.")
	}

	// This API returns a netapp.VolumesCreateOrUpdateFuture, which is some kind of
	// azure.Future thing that is an abstraction for monitoring long-running operations.
	// We currently probe the state of the create operation ourselves using custom backoff
	// code.  Will ignore this struct for the time being, but we may want to look
	// into what interesting features the Azure method may provide.
	// TBD
	if _, err = d.SDKClient.VolumesClient.CreateOrUpdate(d.SDKClient.Ctx, newVol,
		resourceGroup, netAppAccount, cpoolName, request.Name); err != nil {
		return nil, err
	}

	filesystem, err := d.newFileSystemFromVolume(&newVol, cookie)
	if err != nil {
		return nil, err
	}

	log.WithFields(log.Fields{
		"name":          request.Name,
		"creationToken": request.CreationToken,
	}).Info("Filesystem created.")

	return filesystem, nil
}

// RenameVolume is probably not supported on Azure?
func (d *Client) RenameVolume(filesystem *FileSystem, newName string) (*FileSystem, error) {
	return nil, fmt.Errorf("unimplemented")
}

// RelabelVolume updates the 'trident' telemetry label on a volume
func (d *Client) RelabelVolume(filesystem *FileSystem, labels []string) (*FileSystem, error) {

	// Only updating trident labels
	if len(labels) > 1 {
		log.Errorf("Too many labels (%d) passed to RelabelVolume.", len(labels))
	}

	cookie, err := d.GetCookieByCapacityPoolName(filesystem.CapacityPoolName)
	if err != nil {
		return nil, fmt.Errorf("couldn't find cookie for volume: %v on cpool %v",
			filesystem.Name, filesystem.CapacityPoolName)
	}

	// Fetch the netapp.Volume to fill in the new Tags field
	nv, err := d.SDKClient.VolumesClient.Get(d.SDKClient.Ctx, *cookie.ResourceGroup,
		*cookie.NetAppAccount, filesystem.CapacityPoolName, filesystem.Name)
	if err != nil {
		return nil, errors.New("couldn't get volume for RelabelVolume")
	}

	tags := make(map[string]string)
	// Copy any existing tags first in order to make sure to preserve any custom tags that might've
	// been applied prior to a volume import
	if nv.Tags != nil {
		m := nv.Tags.(map[string]interface{})
		for idx, val := range m {
			switch vv := val.(type) {
			case string:
				tags[idx] = vv
			default:
				return nil, errors.New("strange tag type detected in existing tags")
			}
		}
	}
	// Now update the working copy with the incoming change
	for _, val := range labels {
		tags[tridentLabelTag] = val
	}
	nv.Tags = tags

	// Workaround: nv.BaremetalTenantID is an Azure-supplied field, but we find that sometimes its
	// length exceeds limits imposed by Azure's own validation rules.  This is not something we care
	// about, nor can we even change it, so in order to fix the validation issues we nuke this field
	// here before "updating" the volume.
	nv.BaremetalTenantID = nil

	// ProvisioningState is a ReadOnly field, so don't send a value.
	nv.ProvisioningState = nil

	// Clear out other fields that we don't want to change when merely relabeling.
	nv.ServiceLevel = ""
	nv.ExportPolicy = nil
	nv.ProtocolTypes = nil
	nv.MountTargets = nil

	log.WithFields(log.Fields{
		"name":          nv.Name,
		"creationToken": nv.CreationToken,
		"tags":          nv.Tags,
	}).Info("Relabeling filesystem.")

	if _, err = d.SDKClient.VolumesClient.CreateOrUpdate(d.SDKClient.Ctx, nv, *cookie.ResourceGroup,
		*cookie.NetAppAccount, filesystem.CapacityPoolName, filesystem.Name); err != nil {
		return nil, err
	}

	fs, err := d.newFileSystemFromVolume(&nv, cookie)
	if err != nil {
		return nil, err
	}

	log.WithFields(log.Fields{
		"name":          fs.Name,
		"creationToken": fs.CreationToken,
	}).Info("Filesystem relabeled.")

	return fs, nil
}

// RenameRelabelVolume is probably not supported on Azure
func (d *Client) RenameRelabelVolume(filesystem *FileSystem, newName string, labels []string) (*FileSystem, error) {
	return nil, fmt.Errorf("unimplemented")
}

// ResizeVolume sends a VolumePatch to update the Quota
func (d *Client) ResizeVolume(filesystem *FileSystem, newSizeBytes int64) (*FileSystem, error) {
	cookie, err := d.GetCookieByCapacityPoolName(filesystem.CapacityPoolName)
	if err != nil {
		return nil, fmt.Errorf("couldn't find cookie for volume: %v on cpool %v",
			filesystem.Name, filesystem.CapacityPoolName)
	}

	patchprop := netapp.VolumePatchProperties{
		UsageThreshold: &newSizeBytes,
	}

	patch := netapp.VolumePatch{
		ID:                    &filesystem.ID,
		Location:              &filesystem.Location,
		Name:                  &filesystem.Name,
		VolumePatchProperties: &patchprop,
	}

	newVol, err := d.SDKClient.VolumesClient.Update(d.SDKClient.Ctx, patch, *cookie.ResourceGroup,
		*cookie.NetAppAccount, *cookie.CapacityPoolName, filesystem.Name)
	if err != nil {
		return nil, err
	}

	newFS, err := d.newFileSystemFromVolume(&newVol, cookie)
	if err != nil {
		return nil, err
	}

	return newFS, nil
}

// DeleteVolume deletes a volume
func (d *Client) DeleteVolume(filesystem *FileSystem) error {
	cookie, err := d.GetCookieByCapacityPoolName(filesystem.CapacityPoolName)
	if err != nil {
		return fmt.Errorf("couldn't find cookie for volume: %v on cpool %v",
			filesystem.Name, filesystem.CapacityPoolName)
	}

	if _, err = d.SDKClient.VolumesClient.Delete(d.SDKClient.Ctx, *cookie.ResourceGroup,
		*cookie.NetAppAccount, *cookie.CapacityPoolName, filesystem.Name); err != nil {
		return fmt.Errorf("error deleting volume: %v", err)
	}

	log.WithFields(log.Fields{
		"volume": filesystem.CreationToken,
	}).Info("Filesystem deleted.")

	return nil
}

// GetMountTargetsForVolume returns mount target information for a volume
func (d *Client) GetMountTargetsForVolume(filesystem *FileSystem) (*[]MountTarget, error) {
	cookie, err := d.GetCookieByCapacityPoolName(filesystem.CapacityPoolName)
	if err != nil {
		return nil, fmt.Errorf("couldn't find cookie for volume: %v on cpool %v",
			filesystem.Name, filesystem.CapacityPoolName)
	}

	mtlOuter, err := d.SDKClient.MountTargetsClient.List(d.SDKClient.Ctx,
		*cookie.ResourceGroup, *cookie.NetAppAccount, *cookie.CapacityPoolName,
		filesystem.Name)
	if err != nil {
		return nil, fmt.Errorf("error getting mount target list: %v", err)
	}

	var mounts []MountTarget
	mtl := mtlOuter.Value

	for _, mt := range *mtl {
		var m = MountTarget{
			FileSystemID:      *mt.MountTargetProperties.FileSystemID,
			ProvisioningState: *mt.MountTargetProperties.ProvisioningState,
			Location:          *mt.Location,
			EndIP:             *mt.MountTargetProperties.EndIP,
			Gateway:           *mt.MountTargetProperties.Gateway,
			IPAddress:         *mt.MountTargetProperties.IPAddress,
			MountTargetID:     *mt.MountTargetProperties.MountTargetID,
			Netmask:           *mt.MountTargetProperties.Netmask,
			StartIP:           *mt.MountTargetProperties.StartIP,
		}
		if mt.MountTargetProperties.Subnet != nil {
			m.Subnet = *mt.MountTargetProperties.Subnet
		}
		mounts = append(mounts, m)
	}

	return &mounts, nil
}

// GetSnapshotsForVolume returns a list of snapshots on a volume
func (d *Client) GetSnapshotsForVolume(filesystem *FileSystem) (*[]Snapshot, error) {
	cookie, err := d.GetCookieByCapacityPoolName(filesystem.CapacityPoolName)
	if err != nil {
		return nil, fmt.Errorf("couldn't find cookie for volume: %v on cpool %v",
			filesystem.Name, filesystem.CapacityPoolName)
	}

	slist, err := d.SDKClient.SnapshotsClient.List(d.SDKClient.Ctx, *cookie.ResourceGroup,
		*cookie.NetAppAccount, *cookie.CapacityPoolName, filesystem.Name)
	if err != nil {
		return nil, fmt.Errorf("error listing snapshots on volume %s: %v", filesystem.Name, err)
	}

	var snapshots []Snapshot
	snaps := *slist.Value

	for _, ns := range snaps {
		s := Snapshot{
			ProvisioningState: *ns.ProvisioningState,
			Name:              volumeShortname(*ns.Name),
			// OwnerID <> "ID"?
			Location: *ns.Location,
			// no such field: UsedBytes:
		}
		if ns.SnapshotProperties.Created != nil {
			s.Created = *ns.SnapshotProperties.Created
		}
		if ns.SnapshotProperties.FileSystemID != nil {
			s.FileSystemID = *ns.SnapshotProperties.FileSystemID
		}
		if ns.SnapshotProperties.SnapshotID != nil {
			s.SnapshotID = *ns.SnapshotProperties.SnapshotID
		}
		snapshots = append(snapshots, s)
	}

	return &snapshots, nil
}

// GetSnapshotForVolume fetches a specific snaphot on a volume by its name
func (d *Client) GetSnapshotForVolume(filesystem *FileSystem, snapshotName string) (*Snapshot, error) {
	snapshots, err := d.GetSnapshotsForVolume(filesystem)
	if err != nil {
		return nil, err
	}

	for _, snap := range *snapshots {
		if snap.Name == snapshotName {
			return &snap, nil
		}
	}

	return nil, fmt.Errorf("snapshot '%s' not found on volume '%s'", snapshotName, filesystem.Name)
}

// GetSnapshotByID fetches a specific snapshot on a volume by its ID
func (d *Client) GetSnapshotByID(snapshotID string, filesystem *FileSystem) (*Snapshot, error) {
	snapshots, err := d.GetSnapshotsForVolume(filesystem)
	if err != nil {
		return nil, err
	}

	for _, snap := range *snapshots {
		if snap.SnapshotID == snapshotID {
			return &snap, nil
		}
	}

	return nil, fmt.Errorf("no snapshot with ID '%s' found on volume '%s'", snapshotID, filesystem.Name)
}

// WaitForSnapshotState waits for a desired snapshot state and returns once that state is achieved
func (d *Client) WaitForSnapshotState(
	snapshot *Snapshot, filesystem *FileSystem, desiredState string, abortStates []string, maxElapsedTime time.Duration,
) error {
	checkSnapshotState := func() error {
		s, err := d.GetSnapshotForVolume(filesystem, snapshot.Name)

		// Same rigamarole as with deleted volumes: if we are trying to delete, and
		// suddenly can't query the snapshot, check and see if it's a 404.
		if err != nil {
			if desiredState == StateDeleted {
				cookie, err := d.GetCookieByCapacityPoolName(filesystem.CapacityPoolName)
				if err != nil {
					return fmt.Errorf("waitForSnapshotState internal error re-checking snapshot: %v", err)
				}
				snap, err := d.SDKClient.SnapshotsClient.Get(d.SDKClient.Ctx, *cookie.ResourceGroup,
					*cookie.NetAppAccount, *cookie.CapacityPoolName, filesystem.Name, snapshot.Name)
				if err != nil && snap.StatusCode == 404 {
					// Deleted!
					log.Debugf("Implied deletion for snapshot %s.", snapshot.Name)
					return nil
				} else {
					return fmt.Errorf("waitForSnapshotState internal error re-checking snapshot: %v", err)
				}
			}

			return fmt.Errorf("could not get snapshot status; %v", err)
		}

		if s.ProvisioningState == desiredState {
			return nil
		}

		err = fmt.Errorf("snapshot state is %s, not %s", s.ProvisioningState, desiredState)

		// Return a permanent error to stop retrying if we reached one of the abort states
		for _, abortState := range abortStates {
			if s.ProvisioningState == abortState {
				return backoff.Permanent(TerminalState(err))
			}
		}

		return err
	}

	stateNotify := func(err error, duration time.Duration) {
		log.WithFields(log.Fields{
			"increment": duration,
			"message":   err.Error(),
		}).Debugf("Waiting for snapshot state.")
	}
	stateBackoff := backoff.NewExponentialBackOff()
	stateBackoff.MaxElapsedTime = maxElapsedTime
	stateBackoff.MaxInterval = 5 * time.Second
	stateBackoff.RandomizationFactor = 0.1
	stateBackoff.InitialInterval = 2 * time.Second
	stateBackoff.Multiplier = 1.414

	log.WithField("desiredState", desiredState).Info("Waiting for snapshot state.")

	if err := backoff.RetryNotify(checkSnapshotState, stateBackoff, stateNotify); err != nil {
		if terminalStateErr, ok := err.(*TerminalStateError); ok {
			log.Errorf("Snapshot reached terminal state: %v.", terminalStateErr)
		} else {
			log.Errorf("Snapshot state was not %s after %3.2f seconds.",
				desiredState, stateBackoff.MaxElapsedTime.Seconds())
		}
		return err
	}

	log.WithField("desiredState", desiredState).Debugf("Desired snapshot state reached.")

	return nil
}

// CreateSnapshot creates a new snapshot
func (d *Client) CreateSnapshot(request *SnapshotCreateRequest) (*Snapshot, error) {

	fs := *request.Volume

	cookie, err := d.GetCookieByCapacityPoolName(fs.CapacityPoolName)
	if err != nil {
		return nil, fmt.Errorf("couldn't find cookie for volume: %v on cpool %v",
			fs.Name, fs.CapacityPoolName)
	}

	var snap = netapp.Snapshot{
		Location: &request.Location,
		Name:     &request.Name,
	}

	// This returns another "future" object..
	if _, err = d.SDKClient.SnapshotsClient.Create(d.SDKClient.Ctx, snap, *cookie.ResourceGroup,
		*cookie.NetAppAccount, fs.CapacityPoolName, fs.Name, request.Name); err != nil {
		return nil, err
	}

	// This is a bit weird but the API wants to return a *Snapshot
	// We don't actually get anything but the mysterious 'future' object back in Azure.
	// TBD
	var pendingSnap = Snapshot{
		Name:     request.Name,
		Location: request.Location,
	}

	return &pendingSnap, nil
}

// RestoreSnapshot does not seem to have an API on Azure, unless it's the mysterious 'Do'
func (d *Client) RestoreSnapshot(filesystem *FileSystem, snapshot *Snapshot) error {
	log.Errorf("Restore snapshot not implemented in ANF.")
	return nil
}

// DeleteSnapshot deletes a snapshot
func (d *Client) DeleteSnapshot(filesystem *FileSystem, snapshot *Snapshot) error {
	cookie, err := d.GetCookieByCapacityPoolName(filesystem.CapacityPoolName)
	if err != nil {
		return fmt.Errorf("couldn't find cookie for volume: %v on cpool %v",
			filesystem.Name, filesystem.CapacityPoolName)
	}

	// Another "future" object
	_, err = d.SDKClient.SnapshotsClient.Delete(d.SDKClient.Ctx, *cookie.ResourceGroup,
		*cookie.NetAppAccount, filesystem.CapacityPoolName, filesystem.Name, snapshot.Name)

	return err
}

func IsTransitionalState(volumeState string) bool {
	switch volumeState {
	case StateCreating, StateDeleting:
		return true
	case StateInProgress:
		log.Error("***** FOUND InProgress VOLUME STATE! *****")
		return true
	default:
		return false
	}
}

// TerminalStateError signals that the object is in a terminal state.  This is used to stop waiting on
// an object to change state.
type TerminalStateError struct {
	Err error
}

func (e *TerminalStateError) Error() string {
	return e.Err.Error()
}

// TerminalState wraps the given err in a *TerminalStateError.
func TerminalState(err error) *TerminalStateError {
	return &TerminalStateError{
		Err: err,
	}
}
