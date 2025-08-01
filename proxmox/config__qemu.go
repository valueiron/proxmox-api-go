package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Telmate/proxmox-api-go/internal/util"
)

// Currently ZFS local, LVM, Ceph RBD, CephFS, Directory and virtio-scsi-pci are considered.
// Other formats are not verified, but could be added if they're needed.
// const rxStorageTypes = `(zfspool|lvm|rbd|cephfs|dir|virtio-scsi-pci)`

type (
	// TODO phase this out
	QemuDevices map[int]map[string]interface{}
	// TODO phase this out
	QemuDevice map[string]interface{}
	// TODO phase this out
	QemuDeviceParam []string
)

// ConfigQemu - Proxmox API QEMU options
type ConfigQemu struct {
	ID               *GuestID              `json:"id,omitempty"`   // Required for creation, cannot be changed
	Node             *NodeName             `json:"node,omitempty"` // Required for creation
	Agent            *QemuGuestAgent       `json:"agent,omitempty"`
	Args             string                `json:"args,omitempty"`
	Bios             string                `json:"bios,omitempty"`
	Boot             string                `json:"boot,omitempty"`     // TODO should be an array of custom enums
	BootDisk         string                `json:"bootdisk,omitempty"` // TODO discuss deprecation? Only returned as it's deprecated in the proxmox api
	CPU              *QemuCPU              `json:"cpu,omitempty"`      // never nil when returned
	CloudInit        *CloudInit            `json:"cloudinit,omitempty"`
	Description      *string               `json:"description,omitempty"` // never nil when returned
	Disks            *QemuStorages         `json:"disks,omitempty"`
	EFIDisk          QemuDevice            `json:"efidisk,omitempty"`   // TODO should be a struct
	FullClone        *int                  `json:"fullclone,omitempty"` // Deprecated
	HaGroup          string                `json:"hagroup,omitempty"`
	HaState          string                `json:"hastate,omitempty"` // TODO should be custom type with enum
	Hookscript       string                `json:"hookscript,omitempty"`
	Hotplug          string                `json:"hotplug,omitempty"`   // TODO should be a struct
	Iso              *IsoFile              `json:"iso,omitempty"`       // Same as Disks.Ide.Disk_2.CdRom.Iso
	LinkedID         *GuestID              `json:"linked_id,omitempty"` // Only returned setting it has no effect
	Machine          string                `json:"machine,omitempty"`   // TODO should be custom type with enum
	Memory           *QemuMemory           `json:"memory,omitempty"`
	Name             *GuestName            `json:"name,omitempty"` // never nil when returned
	Networks         QemuNetworkInterfaces `json:"networks,omitempty"`
	Onboot           *bool                 `json:"onboot,omitempty"`
	Pool             *PoolName             `json:"pool,omitempty"`
	Protection       *bool                 `json:"protection,omitempty"` // never nil when returned
	QemuDisks        QemuDevices           `json:"disk,omitempty"`       // Deprecated use Disks *QemuStorages instead
	QemuIso          string                `json:"qemuiso,omitempty"`    // Deprecated use Iso *IsoFile instead
	QemuKVM          *bool                 `json:"kvm,omitempty"`
	QemuOs           string                `json:"ostype,omitempty"`
	PciDevices       QemuPciDevices        `json:"pci_devices,omitempty"`
	QemuPxe          bool                  `json:"pxe,omitempty"`
	QemuUnusedDisks  QemuDevices           `json:"unused,omitempty"` // TODO should be a struct
	USBs             QemuUSBs              `json:"usbs,omitempty"`
	QemuVga          QemuDevice            `json:"vga,omitempty"`    // TODO should be a struct
	Scsihw           string                `json:"scsihw,omitempty"` // TODO should be custom type with enum
	Serials          SerialInterfaces      `json:"serials,omitempty"`
	Smbios1          string                `json:"smbios1,omitempty"` // TODO should be custom type with enum?
	Startup          string                `json:"startup,omitempty"` // TODO should be a struct?
	Storage          string                `json:"storage,omitempty"` // this value is only used when doing a full clone and is never returned
	TPM              *TpmState             `json:"tpm,omitempty"`
	Tablet           *bool                 `json:"tablet,omitempty"` // never nil when returned
	Tags             *Tags                 `json:"tags,omitempty"`
	RandomnessDevice *VirtIoRNG            `json:"randomness_device,omitempty"`
}

const (
	ConfigQemu_Error_UnableToUpdateWithoutReboot string = "unable to update vm without rebooting"
	ConfigQemu_Error_CpuRequired                 string = "cpu is required during creation"
	ConfigQemu_Error_MemoryRequired              string = "memory is required during creation"
	ConfigQemu_Error_NodeRequired                string = "node is required during creation"
)

// Create - Tell Proxmox API to make the VM
func (config ConfigQemu) Create(ctx context.Context, client *Client) (*VmRef, error) {
	version, err := client.Version(ctx)
	if err != nil {
		return nil, err
	}
	if err = config.Validate(nil, version); err != nil {
		return nil, err
	}

	var params map[string]interface{}
	_, params, err = config.mapToAPI(ConfigQemu{}, version)
	if err != nil {
		return nil, err
	}
	// pool field unsupported by /nodes/%s/vms/%d/config used by update (currentConfig != nil).
	// To be able to create directly in a configured pool, add pool to mapped params from ConfigQemu, before creating VM
	var pool PoolName
	if config.Pool != nil && *config.Pool != "" {
		params["pool"] = *config.Pool
	}
	var id GuestID
	var node NodeName
	if config.Node != nil {
		node = *config.Node
	}
	url := "/nodes/" + node.String() + "/qemu"
	if config.ID == nil {
		id, err = guestCreateLoop(ctx, "vmid", url, params, client)
		if err != nil {
			return nil, err
		}
	} else {
		id = *config.ID
		params["vmid"] = int(id)
		var exitStatus string
		exitStatus, err = client.PostWithTask(ctx, params, url)
		if err != nil {
			return nil, fmt.Errorf("error creating VM: %v, error status: %s (params: %v)", err, exitStatus, params)
		}
	}

	vmr := &VmRef{
		node:   node,
		vmId:   id,
		pool:   pool,
		vmType: vmRefQemu,
	}
	if err = resizeNewDisks(ctx, vmr, client, config.Disks, nil); err != nil {
		return nil, err
	}
	if err = client.insertCachedPermission(ctx, permissionPath(permissionCategory_GuestPath)+"/"+permissionPath(vmr.vmId.String())); err != nil {
		return nil, err
	}
	_, err = client.UpdateVMHA(ctx, vmr, config.HaState, config.HaGroup)
	return vmr, err
}

// TODO this should not be done here, but should be done in the unmarshaling of eache respective field
func (config *ConfigQemu) defaults() {
	if config == nil {
		return
	}
	if config.Boot == "" {
		config.Boot = "cdn"
	}
	if config.Bios == "" {
		config.Bios = "seabios"
	}
	if config.EFIDisk == nil {
		config.EFIDisk = QemuDevice{}
	}
	if config.Onboot == nil {
		config.Onboot = util.Pointer(true)
	}
	if config.Hotplug == "" {
		config.Hotplug = "network,disk,usb"
	}
	if config.QemuDisks == nil {
		config.QemuDisks = QemuDevices{}
	}
	if config.QemuKVM == nil {
		config.QemuKVM = util.Pointer(true)
	}
	if config.QemuOs == "" {
		config.QemuOs = "other"
	}
	if config.QemuUnusedDisks == nil {
		config.QemuUnusedDisks = QemuDevices{}
	}
	if config.QemuVga == nil {
		config.QemuVga = QemuDevice{}
	}
	if config.Scsihw == "" {
		config.Scsihw = "lsi"
	}
}

func (config ConfigQemu) mapToAPI(currentConfig ConfigQemu, version Version) (rebootRequired bool, params map[string]interface{}, err error) {
	// TODO check if cloudInit settings changed, they require a reboot to take effect.
	var itemsToDelete string

	params = map[string]any{}

	var guestID GuestID
	if config.ID != nil {
		guestID = *config.ID
	}
	if config.Args != "" {
		params["args"] = config.Args
	}
	if config.Agent != nil {
		params[qemuApiKeyGuestAgent] = config.Agent.mapToAPI(currentConfig.Agent)
	}
	if config.Bios != "" {
		params["bios"] = config.Bios
	}
	if config.Boot != "" {
		params["boot"] = config.Boot
	}
	if config.Description != nil && (*config.Description != "" || currentConfig.Description != nil) {
		params[qemuApiKeyDescription] = *config.Description
	}
	if config.Hookscript != "" {
		params["hookscript"] = config.Hookscript
	}
	if config.Hotplug != "" {
		params["hotplug"] = config.Hotplug
	}
	if config.QemuKVM != nil {
		params["kvm"] = *config.QemuKVM
	}
	if config.Machine != "" {
		params["machine"] = config.Machine
	}
	if config.Name != nil {
		if currentConfig.Name == nil || *config.Name != *currentConfig.Name {
			params[qemuApiKeyName] = config.Name.String()
		}
	}
	if config.Onboot != nil {
		params["onboot"] = *config.Onboot
	}
	if config.Protection != nil {
		params["protection"] = *config.Protection
	}
	if config.QemuOs != "" {
		params["ostype"] = config.QemuOs
	}
	if config.Scsihw != "" {
		params["scsihw"] = config.Scsihw
	}
	if config.Startup != "" {
		params["startup"] = config.Startup
	}
	if config.Tablet != nil {
		params[qemuApiKeyTablet] = *config.Tablet
	}
	if config.Tags != nil {
		if v, ok := config.Tags.mapToApiUpdate(currentConfig.Tags); ok {
			params[qemuApiKeyTags] = v
		}
	}
	if config.Smbios1 != "" {
		params["smbios1"] = config.Smbios1
	}
	if config.TPM != nil {
		if delete := config.TPM.mapToApi(params, currentConfig.TPM); delete != "" {
			itemsToDelete = AddToList(itemsToDelete, delete)
		}
	}

	if config.Iso != nil {
		if config.Disks == nil {
			config.Disks = &QemuStorages{}
		}
		if config.Disks.Ide == nil {
			config.Disks.Ide = &QemuIdeDisks{}
		}
		if config.Disks.Ide.Disk_2 == nil {
			config.Disks.Ide.Disk_2 = &QemuIdeStorage{}
		}
		if config.Disks.Ide.Disk_2.CdRom == nil {
			config.Disks.Ide.Disk_2.CdRom = &QemuCdRom{Iso: config.Iso}
		}
	}
	// Disks
	if currentConfig.Disks != nil {
		if config.Disks != nil {
			// Create,Update,Delete
			var linkedID GuestID
			if currentConfig.LinkedID != nil {
				linkedID = *currentConfig.LinkedID
			}
			delete := config.Disks.mapToApiValues(*currentConfig.Disks, guestID, linkedID, params)
			if delete != "" {
				itemsToDelete = AddToList(itemsToDelete, delete)
			}
		}
	} else {
		if config.Disks != nil {
			// Create
			config.Disks.mapToApiValues(QemuStorages{}, guestID, 0, params)
		}
	}

	if config.CPU != nil {
		itemsToDelete += config.CPU.mapToApi(currentConfig.CPU, params, version)
	}
	if config.CloudInit != nil {
		itemsToDelete += config.CloudInit.mapToAPI(currentConfig.CloudInit, params, version)
	}
	if config.Memory != nil {
		itemsToDelete += config.Memory.mapToAPI(currentConfig.Memory, params)
	}
	if config.Serials != nil {
		itemsToDelete += config.Serials.mapToAPI(currentConfig.Serials, params)
	}

	if config.RandomnessDevice != nil {
		if currentConfig.RandomnessDevice != nil {
			itemsToDelete += config.RandomnessDevice.mapToAPIUpdateUnsafe(currentConfig.RandomnessDevice, params)
		} else {
			config.RandomnessDevice.mapToAPICreate(params)
		}
	}

	// Create EFI disk
	config.CreateQemuEfiParams(params)

	// Create networks config.
	itemsToDelete += config.Networks.mapToAPI(currentConfig.Networks, params)

	// Create vga config.
	vgaParam := QemuDeviceParam{}
	vgaParam = vgaParam.createDeviceParam(config.QemuVga, nil)
	if len(vgaParam) > 0 {
		params["vga"] = strings.Join(vgaParam, ",")
	}

	if config.USBs != nil {
		itemsToDelete += config.USBs.mapToAPI(currentConfig.USBs, params)
	}

	if config.PciDevices != nil {
		itemsToDelete += config.PciDevices.mapToAPI(currentConfig.PciDevices, params)
	}

	if itemsToDelete != "" {
		params["delete"] = strings.TrimPrefix(itemsToDelete, ",")
	}
	return
}

func (config *ConfigQemu) mapToStruct(vmr *VmRef, params map[string]interface{}) error {
	// vmConfig Sample: map[ cpu:host
	// net0:virtio=62:DF:XX:XX:XX:XX,bridge=vmbr0
	// ide2:local:iso/xxx-xx.iso,media=cdrom memory:2048
	// smbios1:uuid=8b3bf833-aad8-4545-xxx-xxxxxxx digest:aa6ce5xxxxx1b9ce33e4aaeff564d4 sockets:1
	// name:terraform-ubuntu1404-template bootdisk:virtio0
	// virtio0:ProxmoxxxxISCSI:vm-1014-disk-2,size=4G
	// description:Base image
	// cores:2 ostype:l26

	if vmr != nil {
		if vmr.node != "" {
			nodeCopy := vmr.node
			config.Node = &nodeCopy
		}
		if vmr.pool != "" {
			poolCopy := PoolName(vmr.pool)
			config.Pool = &poolCopy
		}
		if vmr.vmId != 0 {
			idCopy := vmr.vmId
			config.ID = &idCopy
		}
	}

	if _, isSet := params["args"]; isSet {
		config.Args = strings.TrimSpace(params["args"].(string))
	}
	//boot by default from hard disk (c), CD-ROM (d), network (n).
	if _, isSet := params["boot"]; isSet {
		config.Boot = params["boot"].(string)
	}
	if _, isSet := params["bootdisk"]; isSet {
		config.BootDisk = params["bootdisk"].(string)
	}
	if _, isSet := params["bios"]; isSet {
		config.Bios = params["bios"].(string)
	}
	//Can be network,disk,cpu,memory,usb
	if _, isSet := params["hotplug"]; isSet {
		config.Hotplug = params["hotplug"].(string)
	}
	if _, isSet := params["hookscript"]; isSet {
		config.Hookscript = params["hookscript"].(string)
	}
	if _, isSet := params["machine"]; isSet {
		config.Machine = params["machine"].(string)
	}
	if _, isSet := params["onboot"]; isSet {
		config.Onboot = util.Pointer(Itob(int(params["onboot"].(float64))))
	}
	if itemValue, isSet := params["tpmstate0"]; isSet {
		config.TPM = TpmState{}.mapToSDK(itemValue.(string))
	}
	if _, isSet := params["kvm"]; isSet {
		config.QemuKVM = util.Pointer(Itob(int(params["kvm"].(float64))))
	}
	if _, isSet := params["ostype"]; isSet {
		config.QemuOs = params["ostype"].(string)
	}
	if _, isSet := params["scsihw"]; isSet {
		config.Scsihw = params["scsihw"].(string)
	}
	if _, isSet := params["startup"]; isSet {
		config.Startup = params["startup"].(string)
	}
	if _, isSet := params["smbios1"]; isSet {
		config.Smbios1 = params["smbios1"].(string)
	}

	if config.Disks != nil && config.Disks.Ide != nil && config.Disks.Ide.Disk_2 != nil && config.Disks.Ide.Disk_2.CdRom != nil {
		config.Iso = config.Disks.Ide.Disk_2.CdRom.Iso
	}

	// Add unused disks
	// unused0:local:100/vm-100-disk-1.qcow2
	unusedDiskNames := []string{}
	for k := range params {
		// look for entries from the config in the format "unusedX:<storagepath>" where X is an integer
		if unusedDiskName := rxUnusedDiskName.FindStringSubmatch(k); len(unusedDiskName) > 0 {
			unusedDiskNames = append(unusedDiskNames, unusedDiskName[0])
		}
	}
	// if len(unusedDiskNames) > 0 {
	// 	log.Printf("[DEBUG] unusedDiskNames: %v", unusedDiskNames)
	// }

	if len(unusedDiskNames) > 0 {
		config.QemuUnusedDisks = QemuDevices{}
		for _, unusedDiskName := range unusedDiskNames {
			unusedDiskConfStr := params[unusedDiskName].(string)
			finalDiskConfMap := QemuDevice{}

			// parse "unused0" to get the id '0' as an int
			id := rxDeviceID.FindStringSubmatch(unusedDiskName)
			diskID, err := strconv.Atoi(id[0])
			if err != nil {
				return fmt.Errorf("unable to parse unused disk id from input string '%v' tried to convert '%v' to integer", unusedDiskName, diskID)
			}
			finalDiskConfMap["slot"] = diskID

			// parse the attributes from the unused disk
			// extract the storage and file path from the unused disk entry
			parsedUnusedDiskMap := ParsePMConf(unusedDiskConfStr, "storage+file")
			storageName, fileName := ParseSubConf(parsedUnusedDiskMap["storage+file"].(string), ":")
			finalDiskConfMap["storage"] = storageName
			finalDiskConfMap["file"] = fileName

			config.QemuUnusedDisks[diskID] = finalDiskConfMap
			config.QemuUnusedDisks[diskID] = finalDiskConfMap
			config.QemuUnusedDisks[diskID] = finalDiskConfMap
		}
	}
	//Display

	if vga, isSet := params["vga"]; isSet {
		vgaList := strings.Split(vga.(string), ",")
		vgaMap := QemuDevice{}

		vgaMap.readDeviceConfig(vgaList)
		if len(vgaMap) > 0 {
			config.QemuVga = vgaMap
		}
	}

	// efidisk
	if efidisk, isSet := params["efidisk0"].(string); isSet {
		efiDiskConfMap := ParsePMConf(efidisk, "volume")
		storageName, fileName := ParseSubConf(efiDiskConfMap["volume"].(string), ":")
		efiDiskConfMap["storage"] = storageName
		efiDiskConfMap["file"] = fileName
		config.EFIDisk = efiDiskConfMap
	}

	return nil
}

func (config ConfigQemu) Update(ctx context.Context, rebootIfNeeded bool, vmr *VmRef, client *Client) (rebootRequired bool, err error) {
	// TODO add digest during update to check if the config has changed
	// currentConfig will be mutated
	currentConfig, err := NewConfigQemuFromApi(ctx, vmr, client)
	if err != nil {
		return
	}

	if vmr != nil {
		if err = config.setVmr(vmr); err != nil {
			return
		}
	}

	var version Version
	if version, err = client.Version(ctx); err != nil {
		return
	}
	if err = config.Validate(currentConfig, version); err != nil {
		return
	}
	// TODO implement tmp move and version change
	urlPart := "/" + vmr.vmType + "/" + vmr.vmId.String() + "/config"
	var itemsToDeleteBeforeUpdate string // this is for items that should be removed before they can be created again e.g. cloud-init disks. (convert to array when needed)
	stopped := false

	var markedDisks qemuUpdateChanges
	if config.Disks != nil && currentConfig.Disks != nil {
		markedDisks = *config.Disks.markDiskChanges(*currentConfig.Disks)
		for _, e := range markedDisks.Move { // move disk to different storage or change disk format
			_, err = e.move(ctx, true, vmr, client)
			if err != nil {
				return
			}
		}
		if err = resizeDisks(ctx, vmr, client, markedDisks.Resize); err != nil { // increase Disks in size
			return false, err
		}
		itemsToDeleteBeforeUpdate = config.Disks.cloudInitRemove(*currentConfig.Disks)
	}

	if config.TPM != nil && currentConfig.TPM != nil { // delete or move TPM
		delete, disk := config.TPM.markChanges(*currentConfig.TPM)
		if delete != "" { // delete
			itemsToDeleteBeforeUpdate = AddToList(itemsToDeleteBeforeUpdate, delete)
			currentConfig.TPM = nil
		} else if disk != nil { // move
			if _, err := disk.move(ctx, true, vmr, client); err != nil {
				return false, err
			}
		}
	}

	if itemsToDeleteBeforeUpdate != "" {
		err = client.Put(ctx, map[string]interface{}{"delete": itemsToDeleteBeforeUpdate}, "/nodes/"+vmr.node.String()+urlPart)
		if err != nil {
			return false, fmt.Errorf("error updating VM: %v", err)
		}
		// Deleting these items can create pending changes
		rebootRequired, err = GuestHasPendingChanges(ctx, vmr, client)
		if err != nil {
			return
		}
		if rebootRequired { // shutdown vm if reboot is required
			if rebootIfNeeded {
				if err = GuestShutdown(ctx, vmr, client, true); err != nil {
					return
				}
				stopped = true
				rebootRequired = false
			} else {
				return rebootRequired, errors.New(ConfigQemu_Error_UnableToUpdateWithoutReboot)
			}
		}
	}

	// TODO GuestHasPendingChanges() has the current vm config technically. We can use this to avoid an extra API call.
	if len(markedDisks.Move) != 0 { // Moving disks changes the disk id. we need to get the config again if any disk was moved.
		currentConfig, err = NewConfigQemuFromApi(ctx, vmr, client)
		if err != nil {
			return
		}
	}

	if config.Node != nil && currentConfig.Node != nil && *config.Node != *currentConfig.Node { // Migrate VM
		if err = vmr.migrate_Unsafe(ctx, client, *config.Node, true); err != nil {
			return
		}
		// Set node to the node the VM was migrated to
		vmr.node = *config.Node
	}

	var params map[string]interface{}
	rebootRequired, params, err = config.mapToAPI(*currentConfig, version)
	if err != nil {
		return
	}
	var exitStatus string
	exitStatus, err = client.PutWithTask(ctx, params, "/nodes/"+vmr.node.String()+urlPart)
	if err != nil {
		return false, fmt.Errorf("error updating VM: %v, error status: %s (params: %v)", err, exitStatus, params)
	}

	if !rebootRequired && !stopped { // only check if reboot is required if the vm is not already stopped
		rebootRequired, err = GuestHasPendingChanges(ctx, vmr, client)
		if err != nil {
			return
		}
	}

	if err = resizeNewDisks(ctx, vmr, client, config.Disks, currentConfig.Disks); err != nil {
		return
	}

	if config.Pool != nil { // update pool membership
		guestSetPoolNoCheck(ctx, client, uint(vmr.vmId), *config.Pool, currentConfig.Pool, version)
	}

	if stopped { // start vm if it was stopped
		if rebootIfNeeded {
			if err = GuestStart(ctx, vmr, client); err != nil {
				return
			}
			stopped = false
			rebootRequired = false
		} else {
			return true, nil
		}
	} else if rebootRequired { // reboot vm if it is running
		if rebootIfNeeded {
			if err = GuestReboot(ctx, vmr, client); err != nil {
				return
			}
			rebootRequired = false
		} else {
			return rebootRequired, nil
		}
	}

	_, err = client.UpdateVMHA(ctx, vmr, config.HaState, config.HaGroup)
	return
}

func (config *ConfigQemu) setVmr(vmr *VmRef) (err error) {
	if config == nil {
		return errors.New("config may not be nil")
	}
	if err = vmr.nilCheck(); err != nil {
		return
	}
	vmr.SetVmType("qemu")
	idCopy := vmr.vmId
	config.ID = &idCopy
	return
}

func (config ConfigQemu) Validate(current *ConfigQemu, version Version) (err error) {
	// TODO test all other use cases
	// TODO has no context about changes caused by updating the vm
	if current == nil { // Create
		if config.ID != nil {
			if err = config.ID.Validate(); err != nil {
				return
			}
		}
		if config.Node == nil {
			return errors.New(ConfigQemu_Error_NodeRequired)
		}
		if err = config.Node.Validate(); err != nil {
			return
		}
		if config.CPU == nil {
			return errors.New(ConfigQemu_Error_CpuRequired)
		} else {
			if err = config.CPU.Validate(nil, version); err != nil {
				return
			}
		}
		if config.Memory == nil {
			return errors.New(ConfigQemu_Error_MemoryRequired)
		} else {
			if err = config.Memory.Validate(nil); err != nil {
				return
			}
		}
		if config.Networks != nil {
			if err = config.Networks.Validate(nil); err != nil {
				return
			}
		}
		if config.PciDevices != nil {
			if err = config.PciDevices.Validate(nil); err != nil {
				return
			}
		}
		if config.RandomnessDevice != nil {
			if err = config.RandomnessDevice.validateCreate(); err != nil {
				return
			}
		}
		if config.TPM != nil {
			if err = config.TPM.Validate(nil); err != nil {
				return
			}
		}
		if config.USBs != nil {
			if err = config.USBs.Validate(nil); err != nil {
				return
			}
		}
	} else { // Update
		if config.Node != nil {
			if err = config.Node.Validate(); err != nil {
				return
			}
		}
		if config.CPU != nil {
			if err = config.CPU.Validate(current.CPU, version); err != nil {
				return
			}
		}
		if config.Memory != nil {
			if err = config.Memory.Validate(current.Memory); err != nil {
				return
			}
		}
		if config.Networks != nil {
			if err = config.Networks.Validate(current.Networks); err != nil {
				return
			}
		}
		if config.PciDevices != nil {
			if err = config.PciDevices.Validate(current.PciDevices); err != nil {
				return
			}
		}
		if config.RandomnessDevice != nil {
			if err = config.RandomnessDevice.Validate(current.RandomnessDevice); err != nil {
				return
			}
		}
		if config.TPM != nil {
			if err = config.TPM.Validate(current.TPM); err != nil {
				return
			}
		}
		if config.USBs != nil {
			if err = config.USBs.Validate(current.USBs); err != nil {
				return
			}
		}
	}
	// Shared
	if config.Agent != nil {
		if err = config.Agent.Validate(); err != nil {
			return
		}
	}
	if config.CloudInit != nil {
		if err = config.CloudInit.Validate(version); err != nil {
			return
		}
	}
	if config.Disks != nil {
		err = config.Disks.Validate()
		if err != nil {
			return
		}
	}
	if config.Name != nil {
		if err = config.Name.Validate(); err != nil {
			return
		}
	}
	if config.Pool != nil && *config.Pool != "" {
		if err = config.Pool.Validate(); err != nil {
			return
		}
	}
	if len(config.Serials) > 0 {
		if err = config.Serials.Validate(); err != nil {
			return
		}
	}
	if config.Tags != nil {
		if err := (*config.Tags).Validate(); err != nil {
			return err
		}
	}
	return
}

/*
CloneVm
Example: request

nodes/proxmox1-xx/qemu/1012/clone

newid:145
name:tf-clone1
target:proxmox1-xx
full:1
storage:xxx
*/
// Deprecated: use VmRef.CloneQemu() instead
func (config ConfigQemu) CloneVm(ctx context.Context, sourceVmr *VmRef, vmr *VmRef, client *Client) (err error) {
	vmr.SetVmType("qemu")
	var storage string
	var format string
	fullClone := "1"
	if config.FullClone != nil {
		fullClone = strconv.Itoa(*config.FullClone)
	}
	if disk0Storage, ok := config.QemuDisks[0]["storage"].(string); ok && len(disk0Storage) > 0 {
		storage = disk0Storage
	}
	if disk0Format, ok := config.QemuDisks[0]["format"].(string); ok && len(disk0Format) > 0 {
		format = disk0Format
	}
	params := map[string]interface{}{
		"newid":  vmr.vmId,
		"target": vmr.node,
		"name":   config.Name,
		"full":   fullClone,
	}
	if vmr.pool != "" {
		params["pool"] = vmr.pool
	}

	if fullClone == "1" {
		if storage != "" {
			params["storage"] = storage
		}
		if format != "" {
			params["format"] = format
		}
	}

	_, err = client.CloneQemuVm(ctx, sourceVmr, params)
	return err
}

func NewConfigQemuFromJson(input []byte) (config *ConfigQemu, err error) {
	config = &ConfigQemu{}
	err = json.Unmarshal([]byte(input), config)
	if err != nil {
		log.Fatal(err)
	}
	return
}

var (
	rxDeviceID       = regexp.MustCompile(`\d+`)
	rxUnusedDiskName = regexp.MustCompile(`^(unused)\d+`)
	rxNicName        = regexp.MustCompile(`net\d+`)
	rxMpName         = regexp.MustCompile(`mp\d+`)
)

// Useful waiting for ISO install to complete
func WaitForShutdown(ctx context.Context, vmr *VmRef, client *Client) (err error) {
	for ii := 0; ii < 100; ii++ {
		raw, err := vmr.GetRawGuestStatus(ctx, client)
		if err != nil {
			log.Print("Wait error:")
			log.Println(err)
		} else if raw.State() == PowerStateStopped {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("not shutdown within wait time")
}

// This is because proxmox create/config API won't let us make usernet devices
func SshForwardUsernet(ctx context.Context, vmr *VmRef, client *Client) (sshPort string, err error) {
	raw, err := vmr.GetRawGuestStatus(ctx, client)
	if err != nil {
		return "", err
	}
	if raw.State() == PowerStateStopped {
		return "", fmt.Errorf("VM must be running first")
	}
	sshPort = strconv.Itoa(int(vmr.VmId()) + 22000)
	_, err = client.MonitorCmd(ctx, vmr, "netdev_add user,id=net1,hostfwd=tcp::"+sshPort+"-:22")
	if err != nil {
		return "", err
	}
	_, err = client.MonitorCmd(ctx, vmr, "device_add virtio-net-pci,id=net1,netdev=net1,addr=0x13")
	if err != nil {
		return "", err
	}
	return
}

// device_del net1
// netdev_del net1
func RemoveSshForwardUsernet(ctx context.Context, vmr *VmRef, client *Client) (err error) {
	raw, err := vmr.GetRawGuestStatus(ctx, client)
	if err != nil {
		return err
	}
	if raw.State() == PowerStateStopped {
		return fmt.Errorf("VM must be running first")
	}
	_, err = client.MonitorCmd(ctx, vmr, "device_del net1")
	if err != nil {
		return err
	}
	_, err = client.MonitorCmd(ctx, vmr, "netdev_del net1")
	if err != nil {
		return err
	}
	return nil
}

func MaxVmId(ctx context.Context, client *Client) (max int, err error) {
	vms, err := client.GetResourceList(ctx, resourceListGuest)
	max = 100
	for vmii := range vms {
		vm := vms[vmii].(map[string]interface{})
		vmid := int(vm["vmid"].(float64))
		if vmid > max {
			max = vmid
		}
	}
	return
}

func SendKeysString(ctx context.Context, vmr *VmRef, client *Client, keys string) (err error) {
	raw, err := vmr.GetRawGuestStatus(ctx, client)
	if err != nil {
		return err
	}
	if raw.State() == PowerStateStopped {
		return fmt.Errorf("VM must be running first")
	}
	for _, r := range keys {
		c := string(r)
		lower := strings.ToLower(c)
		if c != lower {
			c = "shift-" + lower
		} else {
			switch c {
			case "!":
				c = "shift-1"
			case "@":
				c = "shift-2"
			case "#":
				c = "shift-3"
			case "$":
				c = "shift-4"
			case "%%":
				c = "shift-5"
			case "^":
				c = "shift-6"
			case "&":
				c = "shift-7"
			case "*":
				c = "shift-8"
			case "(":
				c = "shift-9"
			case ")":
				c = "shift-0"
			case "_":
				c = "shift-minus"
			case "+":
				c = "shift-equal"
			case " ":
				c = "spc"
			case "/":
				c = "slash"
			case "\\":
				c = "backslash"
			case ",":
				c = "comma"
			case "-":
				c = "minus"
			case "=":
				c = "equal"
			case ".":
				c = "dot"
			case "?":
				c = "shift-slash"
			}
		}
		_, err = client.MonitorCmd(ctx, vmr, "sendkey "+c)
		if err != nil {
			return err
		}
		time.Sleep(1 * time.Millisecond)
	}
	return nil
}

// Given a QemuDevice, return a param string to give to ProxMox
func formatDeviceParam(device QemuDevice) string {
	deviceConfParams := QemuDeviceParam{}
	deviceConfParams = deviceConfParams.createDeviceParam(device, nil)
	return strings.Join(deviceConfParams, ",")
}

// Given a QemuDevice (representing a disk), return a param string to give to ProxMox
func FormatDiskParam(disk QemuDevice) string {
	diskConfParam := QemuDeviceParam{}

	if volume, ok := disk["volume"]; ok && volume != "" {
		diskConfParam = append(diskConfParam, volume.(string))

		if size, ok := disk["size"]; ok && size != "" {
			diskConfParam = append(diskConfParam, fmt.Sprintf("size=%v", disk["size"]))
		}
	} else {
		volumeInit := fmt.Sprintf("%v:%v", disk["storage"], DiskSizeGB(disk["size"]))
		diskConfParam = append(diskConfParam, volumeInit)
	}

	// Set cache if not none (default).
	if cache, ok := disk["cache"]; ok && cache != "none" {
		diskCache := fmt.Sprintf("cache=%v", disk["cache"])
		diskConfParam = append(diskConfParam, diskCache)
	}

	// Mountoptions
	if mountoptions, ok := disk["mountoptions"]; ok {
		options := []string{}
		for opt, enabled := range mountoptions.(map[string]interface{}) {
			if enabled.(bool) {
				options = append(options, opt)
			}
		}
		diskMountOpts := fmt.Sprintf("mountoptions=%v", strings.Join(options, ";"))
		diskConfParam = append(diskConfParam, diskMountOpts)
	}

	// Backup
	if backup, ok := disk["backup"].(bool); ok {
		// Backups are enabled by default (backup=1)
		// Only set the parameter if backups are explicitly disabled
		if !backup {
			diskConfParam = append(diskConfParam, "backup=0")
		}
	}

	// Keys that are not used as real/direct conf.
	ignoredKeys := []string{"backup", "key", "slot", "type", "storage", "file", "size", "cache", "volume", "container", "vm", "mountoptions", "storage_type"}

	// Rest of config.
	diskConfParam = diskConfParam.createDeviceParam(disk, ignoredKeys)

	return strings.Join(diskConfParam, ",")
}

// Create efi parameter.
func (c ConfigQemu) CreateQemuEfiParams(params map[string]interface{}) {
	efiParam := QemuDeviceParam{}
	efiParam = efiParam.createDeviceParam(c.EFIDisk, nil)

	if len(efiParam) > 0 {
		storage_info := []string{}
		storage := ""
		for _, param := range efiParam {
			key := strings.Split(param, "=")
			if key[0] == "storage" {
				// Proxmox format for disk creation
				storage = fmt.Sprintf("%s:1", key[1])
			} else {
				storage_info = append(storage_info, param)
			}
		}
		if len(storage_info) > 0 {
			storage = fmt.Sprintf("%s,%s", storage, strings.Join(storage_info, ","))
		}
		params["efidisk0"] = storage
	}
}

func (p QemuDeviceParam) createDeviceParam(
	deviceConfMap QemuDevice,
	ignoredKeys []string,
) QemuDeviceParam {

	for key, value := range deviceConfMap {
		if ignored := slices.Contains(ignoredKeys, key); !ignored {
			var confValue interface{}
			if bValue, ok := value.(bool); ok && bValue {
				confValue = "1"
			} else if sValue, ok := value.(string); ok && len(sValue) > 0 {
				confValue = sValue
			} else if iValue, ok := value.(int); ok && iValue > 0 {
				confValue = iValue
			} else if iValue, ok := value.(float64); ok && iValue > 0 {
				confValue = iValue
			}
			if confValue != nil {
				deviceConf := fmt.Sprintf("%v=%v", key, confValue)
				p = append(p, deviceConf)
			}
		}
	}

	return p
}

// readDeviceConfig - get standard sub-conf strings where `key=value` and update conf map.
func (confMap QemuDevice) readDeviceConfig(confList []string) {
	// Add device config.
	for _, conf := range confList {
		key, value := ParseSubConf(conf, "=")
		confMap[key] = value
	}
}

func (c ConfigQemu) String() string {
	jsConf, _ := json.Marshal(c)
	return string(jsConf)
}

type RawConfigQemu map[string]any

func (raw RawConfigQemu) ALL(vmr *VmRef) (*ConfigQemu, error) {
	config, err := raw.all(vmr)
	if err != nil {
		return nil, err
	}
	config.defaults()
	return config, nil
}

func (raw RawConfigQemu) all(vmr *VmRef) (*ConfigQemu, error) {
	config := ConfigQemu{
		Agent:            raw.Agent(),
		CPU:              raw.CPU(),
		CloudInit:        raw.CloudInit(),
		Description:      util.Pointer(raw.Description()),
		Memory:           raw.Memory(),
		Name:             util.Pointer(raw.Name()),
		Networks:         raw.Networks(),
		PciDevices:       raw.PciDevices(),
		Protection:       util.Pointer(raw.Protection()),
		RandomnessDevice: raw.RandomnessDevice(),
		Serials:          raw.Serials(),
		Tablet:           util.Pointer(raw.Tablet()),
		Tags:             raw.Tags(),
		USBs:             raw.USBs(),
	}
	config.Disks, config.LinkedID = raw.Disks()
	if err := config.mapToStruct(vmr, raw); err != nil {
		return nil, err
	}
	return &config, nil
}

func (raw RawConfigQemu) Description() string {
	if v, isSet := raw[qemuApiKeyDescription]; isSet {
		return v.(string)
	}
	return ""
}

func (raw RawConfigQemu) Name() GuestName {
	if v, isSet := raw[qemuApiKeyName]; isSet {
		return GuestName(v.(string))
	}
	return ""
}

func (raw RawConfigQemu) Protection() bool {
	if v, isSet := raw[qemuApiKeyProtection]; isSet {
		return int(v.(float64)) == 1
	}
	return false
}

func (raw RawConfigQemu) Tablet() bool {
	if v, isSet := raw[qemuApiKeyTablet]; isSet {
		return int(v.(float64)) == 1
	}
	return true
}

func (raw RawConfigQemu) Tags() *Tags {
	if v, isSet := raw[qemuApiKeyTags]; isSet {
		return util.Pointer(Tags{}.mapToSDK(v.(string)))
	}
	return nil
}

const (
	qemuApiKeyCloudInitCustom   string = "cicustom"
	qemuApiKeyCloudInitPassword string = "cipassword"
	qemuApiKeyCloudInitSshKeys  string = "sshkeys"
	qemuApiKeyCloudInitUpgrade  string = "ciupgrade"
	qemuApiKeyCloudInitUser     string = "ciuser"
	qemuApiKeyCpuAffinity       string = "affinity"
	qemuApiKeyCpuCores          string = "cores"
	qemuApiKeyCpuLimit          string = "cpulimit"
	qemuApiKeyCpuNuma           string = "numa"
	qemuApiKeyCpuSockets        string = "sockets"
	qemuApiKeyCpuType           string = "cpu"
	qemuApiKeyCpuUnits          string = "cpuunits"
	qemuApiKeyCpuVirtual        string = "vcpus"
	qemuApiKeyDescription       string = "description"
	qemuApiKeyGuestAgent        string = "agent"
	qemuApiKeyMemoryBallooning  string = "balloon"
	qemuApiKeyMemoryCapacity    string = "memory"
	qemuApiKeyMemoryShares      string = "shares"
	qemuApiKeyName              string = "name"
	qemuApiKeyProtection        string = "protection"
	qemuApiKeyRandomnessDevice  string = "rng0"
	qemuApiKeyTablet            string = "tablet"
	qemuApiKeyTags              string = "tags"
	qemuPrefixApiKeyDiskIde     string = "ide"
	qemuPrefixApiKeyDiskSCSI    string = "scsi"
	qemuPrefixApiKeyDiskSata    string = "sata"
	qemuPrefixApiKeyDiskVirtIO  string = "virtio"
	qemuPrefixApiKeyNetwork     string = "net"
	qemuPrefixApiKeyPCI         string = "hostpci"
	qemuPrefixApiKeySerial      string = "serial"
	qemuPrefixApiKeyUSB         string = "usb"
)

func NewRawConfigQemuFromApi(ctx context.Context, vmr *VmRef, client *Client) (RawConfigQemu, error) {
	rawConfig, err := client.GetVmConfig(ctx, vmr)
	if err != nil {
		return nil, err
	}
	return rawConfig, nil
}

func NewConfigQemuFromApi(ctx context.Context, vmr *VmRef, client *Client) (config *ConfigQemu, err error) {
	var raw RawConfigQemu
	var vmInfo map[string]interface{}
	for ii := 0; ii < 3; ii++ {
		raw, err = NewRawConfigQemuFromApi(ctx, vmr, client)
		if err != nil {
			return nil, err
		}
		// TODO: this is a workaround for the issue that GetVmConfig will not always return the guest info
		vmInfo, err = client.GetVmInfo(ctx, vmr)
		if err != nil {
			return nil, err
		}
		// this can happen:
		// {"data":{"lock":"clone","digest":"eb54fb9d9f120ba0c3bdf694f73b10002c375c38","description":" qmclone temporary file\n"}})
		if vmInfo["lock"] == nil {
			break
		} else {
			time.Sleep(8 * time.Second)
		}
	}

	if vmInfo["lock"] != nil {
		return nil, fmt.Errorf("vm locked, could not obtain config")
	}
	if v, isSet := vmInfo["pool"]; isSet { // TODO: this is a workaround for the issue that GetVmConfig will not always return the guest info
		vmr.pool = PoolName(v.(string))
	}

	config, err = raw.ALL(vmr)
	if err != nil {
		return
	}

	config.defaults()

	// HAstate is return by the api for a vm resource type but not the HAgroup
	err = client.ReadVMHA(ctx, vmr) // TODO: can be optimized, uses same API call as GetVmConfig and GetVmInfo
	if err == nil {
		config.HaState = vmr.HaState()
		config.HaGroup = vmr.HaGroup()
	} else {
		//log.Printf("[DEBUG] VM %d(%s) has no HA config", vmr.vmId, vmConfig["hostname"])
		return config, nil
	}
	return
}
