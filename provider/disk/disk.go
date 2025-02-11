package disk

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/AliyunContainerService/flexvolume/provider/utils"
	"github.com/denverdino/aliyungo/common"
	"github.com/denverdino/aliyungo/ecs"
	"github.com/denverdino/aliyungo/metadata"
	"github.com/nightlyone/lockfile"
	log "github.com/sirupsen/logrus"
)

// Const values for disk
const (
	KUBERNETES_ALICLOUD_DISK_DRIVER = "alicloud_disk"
	VolumeDir                       = "/etc/kubernetes/volumes/disk/"
	VolumeDirRemove                 = "/etc/kubernetes/volumes/disk/remove"
	DISK_AKID                       = "/etc/.volumeak/diskAkId"
	DISK_AKSECRET                   = "/etc/.volumeak/diskAkSecret"
	DISK_ECSENPOINT                 = "/etc/.volumeak/diskEcsEndpoint"
	ECSDEFAULTENDPOINT              = "https://ecs-cn-hangzhou.aliyuncs.com"
)

// DiskOptions define the disk parameters
type DiskOptions struct {
	VolumeName string `json:"kubernetes.io/pvOrVolumeName"`
	FsType     string `json:"kubernetes.io/fsType"`
	VolumeId   string `json:"volumeId"`
}

// the iddentity for http headker
var KUBERNETES_ALICLOUD_IDENTITY = fmt.Sprintf("Kubernetes.Alicloud/Flexvolume.Disk-%s", utils.PluginVersion())
// default region for aliyun sdk usage
var DEFAULT_REGION = common.Hangzhou

// DiskPlugin define DiskPlugin
type DiskPlugin struct {
	client *ecs.Client
}

// NewOptions define NewOptions
func (p *DiskPlugin) NewOptions() interface{} {
	return &DiskOptions{}
}

// Init define Init for DiskPlugin
func (p *DiskPlugin) Init() utils.Result {
	return utils.Succeed()
}

// Attach attach with NodeName and Options
// Attach: nodeName: regionId.instanceId, exammple: cn-hangzhou.i-bp12gei4ljuzilgwzahc
// Attach: options: {"kubernetes.io/fsType": "", "kubernetes.io/pvOrVolumeName": "", "kubernetes.io/readwrite": "", "volumeId":""}
func (p *DiskPlugin) Attach(opts interface{}, nodeName string) utils.Result {

	log.Infof("Disk Plugin Attach: %s", strings.Join(os.Args, ","))

	// Step 0: Check disk is attached on this host
	// resolve kubelet restart issue
	opt := opts.(*DiskOptions)
	cmd := fmt.Sprintf("mount | grep alicloud~disk/%s", opt.VolumeName)
	if out, err := utils.Run(cmd); err == nil {
		devicePath := strings.Split(strings.TrimSpace(out), " ")[0]
		log.Infof("Disk Already Attached, DiskId: %s, Device: %s", opt.VolumeName, devicePath)
		return utils.Result{Status: "Success", Device: devicePath}
	}

	// Step 1: init ecs client and parameters
	p.initEcsClient()
	regionId, instanceId, err := utils.GetRegionAndInstanceId()
	if err != nil {
		utils.FinishError("Disk, Parse node region/name error: " + nodeName + err.Error())
	}
	p.client.SetUserAgent(KUBERNETES_ALICLOUD_DISK_DRIVER + "/" + instanceId)
	attachRequest := &ecs.AttachDiskArgs{
		InstanceId: instanceId,
		DiskId:     opt.VolumeId,
	}

	// Step 2: Detach disk first
	var devicePath string
	describeDisksRequest := &ecs.DescribeDisksArgs{
		DiskIds:  []string{opt.VolumeId},
		RegionId: common.Region(regionId),
	}
	// call detach to ensure work after node reboot
	disks, _, err := p.client.DescribeDisks(describeDisksRequest)
	if err != nil {
		utils.FinishError("Disk, Can not get disk: " + opt.VolumeId + ", with error:" + err.Error())
	}
	if len(disks) >= 1 && disks[0].Status == ecs.DiskStatusInUse {
		err = p.client.DetachDisk(disks[0].InstanceId, disks[0].DiskId)
		if err != nil {
			utils.FinishError("Disk, Failed to detach: " + err.Error())
		}
	}

	// Step 3: wait for Detach
	for i := 0; i < 15; i++ {
		disks, _, err := p.client.DescribeDisks(describeDisksRequest)
		if err != nil {
			utils.FinishError("Could not get Disk again " + opt.VolumeId + ", with error: " + err.Error())
		}
		if len(disks) >= 1 && disks[0].Status == ecs.DiskStatusAvailable {
			break
		}
		if i == 14 {
			utils.FinishError("Detach disk timeout, failed: " + opt.VolumeId)
		}
		time.Sleep(2000 * time.Millisecond)
	}
	log.Infof("Disk is ready to attach: %s, %s, %s", opt.VolumeName, opt.VolumeId, opt.FsType)

	// multi disk attach at the same time
	// lck file created under /tmp/
	lockfileName := "lockfile-disk.lck"
	lock, err := lockfile.New(filepath.Join(os.TempDir(), lockfileName))
	if err != nil {
		utils.FinishError("Lockfile New failed, DiskId: " + opt.VolumeId + ", Volume: " + opt.VolumeName + ", err: " + err.Error())
	}
	err = lock.TryLock()
	if err != nil {
		utils.FinishError("Lockfile failed, DiskId: " + opt.VolumeId + ", Volume: " + opt.VolumeName + ", err: " + err.Error())
	}
	defer lock.Unlock()

	// Step 4: Attach Disk, list device before attach disk
	before := GetCurrentDevices()
	if err = p.client.AttachDisk(attachRequest); err != nil {
		utils.FinishError("Attach failed, DiskId: " + opt.VolumeId + ", Volume: " + opt.VolumeName + ", err: " + err.Error())
	}

	// step 5: wait for attach
	for i := 0; i < 15; i++ {
		disks, _, err := p.client.DescribeDisks(describeDisksRequest)
		if err != nil {
			utils.FinishError("Attach describe error, DiskId: " + opt.VolumeId + ", Volume: " + opt.VolumeName + ", err: " + err.Error())
		}
		if len(disks) >= 1 && disks[0].Status == ecs.DiskStatusInUse {
			break
		}
		if i == 14 {
			utils.FinishError("Attach timeout, DiskId: " + opt.VolumeId + ", Volume: " + opt.VolumeName)
		}
		time.Sleep(2000 * time.Millisecond)
	}

	// Step 6: Analysis attach device, list device after attach device
	for i := 0; i < 15; i++ {
		after := GetCurrentDevices()
		devicePaths := getDevicePath(before, after)
		if i == 9 {
			utils.FinishError("Attach Success, but get DevicePath error1, DiskId: " + opt.VolumeId + ", Volume: " + opt.VolumeName + ", DevicePaths: " + strings.Join(devicePaths, ",") + ", After: " + strings.Join(after, ","))
		}
		if len(devicePaths) == 2 && strings.HasPrefix(devicePaths[1], devicePaths[0]) {
			devicePath = devicePaths[1]
			break
		} else if len(devicePaths) == 1 {
			devicePath = devicePaths[0]
			break
		} else if len(devicePaths) == 0 {
			time.Sleep(2 * time.Second)
		} else {
			utils.FinishError("Attach Success, but get DevicePath error2, DiskId: " + opt.VolumeId + ", Volume: " + opt.VolumeName + ", DevicePaths: " + strings.Join(devicePaths, ",") + ", After: " + strings.Join(after, ","))
		}
	}

	// save volume info to file
	if err := saveVolumeConfig(opt); err != nil {
		log.Errorf("Save volume config failed: " + err.Error())
	}

	log.Infof("Attach successful, DiskId: %s, Volume: %s, Device: %s", opt.VolumeId, opt.VolumeName, devicePath)
	return utils.Result{
		Status: "Success",
		Device: "/dev/" + devicePath,
	}
}

// GetCurrentDevices: Get devices like /dev/vd**
func GetCurrentDevices() []string {
	var devices []string
	files, _ := ioutil.ReadDir("/dev")
	for _, file := range files {
		if !file.IsDir() && strings.Contains(file.Name(), "vd") {
			devices = append(devices, file.Name())
		}
	}
	return devices
}

// Detach current kubelet call detach not provide plugin spec;
// this issue is tracked by: https://github.com/kubernetes/kubernetes/issues/52590
func (p *DiskPlugin) Detach(volumeName string, nodeName string) utils.Result {
	log.Infof("Disk Plugin Detach: %s", strings.Join(os.Args, ","))

	// Step 1: init ecs client
	p.initEcsClient()
	regionId, instanceId, err := utils.GetRegionAndInstanceId()
	if err != nil {
		utils.FinishError("Detach with get regionid/instanceid error: " + err.Error())
	}

	// step 2: get diskid
	diskId := volumeName
	tmpDiskId := getVolumeConfig(volumeName)
	if tmpDiskId != "" && tmpDiskId != volumeName {
		diskId = tmpDiskId
	}

	// Step 3: check disk
	p.client.SetUserAgent(KUBERNETES_ALICLOUD_DISK_DRIVER + "/" + instanceId)
	describeDisksRequest := &ecs.DescribeDisksArgs{
		RegionId: common.Region(regionId),
		DiskIds:  []string{diskId},
	}
	disks, _, err := p.client.DescribeDisks(describeDisksRequest)
	if err != nil {
		utils.FinishError("Failed to list Volume: " + volumeName + ", DiskId: " + diskId + ", with error: " + err.Error())
	}
	if len(disks) == 0 {
		log.Info("No Need Detach, Volume: ", volumeName, ", DiskId: ", diskId, " is not exist")
		return utils.Succeed()
	}

	// Step 4: Detach disk
	disk := disks[0]
	if disk.InstanceId != "" {
		// only detach disk on self instance
		if disk.InstanceId != instanceId {
			log.Info("Skip Detach, Volume: ", volumeName, ", DiskId: ", diskId, " is attached on: ", disk.InstanceId)
			return utils.Succeed()
		}

		// multi disk detach at the same time
		// lck file created under /tmp/
		lockfileName := "lockfile-disk.lck"
		lock, err := lockfile.New(filepath.Join(os.TempDir(), lockfileName))
		if err != nil {
			utils.FinishError("Detach:: Lockfile New failed, DiskId: " + ", Volume: " + volumeName + ", err: " + err.Error())
		}
		err = lock.TryLock()
		if err != nil {
			utils.FinishError("Detach:: Lockfile failed, DiskId: " + volumeName + ", err: " + err.Error())
		}
		defer lock.Unlock()

		err = p.client.DetachDisk(disk.InstanceId, disk.DiskId)
		if err != nil {
			utils.FinishError("Disk, Failed to detach: " + err.Error())
		}
	}

	// step 5: remove volume config file
	removeVolumeConfig(volumeName)

	log.Info("Detach Successful, Volume: ", volumeName, ", DiskId: ", diskId, ", NodeName: ", nodeName)
	return utils.Succeed()
}

// Mount Not Support
func (p *DiskPlugin) Mount(opts interface{}, mountPath string) utils.Result {
	return utils.NotSupport()
}

// Unmount Support, to fix umount bug;
func (p *DiskPlugin) Unmount(mountPoint string) utils.Result {
	log.Infof("Disk, Starting to Unmount: %s", mountPoint)

	p.doUnmount(mountPoint)
	log.Infof("Disk, Unmount Successful: %s", mountPoint)
	return utils.Succeed()
}

func (p *DiskPlugin) doUnmount(mountPoint string) {
	if err := UnmountMountPoint(mountPoint); err != nil {
		utils.FinishError("Disk, Failed to Unmount: " + mountPoint + err.Error())
	}

	// issue: below directory can not be umounted
	// /var/lib/kubelet/plugins/kubernetes.io/flexvolume/alicloud/disk/mounts/d-2zefwuq9sv0gkxqrll5t
	diskMntPath := "/var/lib/kubelet/plugins/kubernetes.io/flexvolume/alicloud/disk/mounts/" + filepath.Base(mountPoint)
	if err := UnmountMountPoint(diskMntPath); err != nil {
		utils.FinishError("Disk, Failed to Unmount: " + diskMntPath + " with error: " + err.Error())
	}
}

// UnmountMountPoint Unmount host mount path
func UnmountMountPoint(mountPath string) error {
	// check mountpath is exist
	if pathExists, pathErr := utils.PathExists(mountPath); pathErr != nil {
		return pathErr
	} else if !pathExists {
		return nil
	}

	// check mountPath is mountPoint
	var notMnt bool
	var err error
	notMnt, err = utils.IsLikelyNotMountPoint(mountPath)
	if err != nil {
		return err
	}
	if notMnt {
		log.Warningf("Warning: %q is not a mountpoint, deleting", mountPath)
		return os.Remove(mountPath)
	}

	// Unmount the mount path
	mntCmd := fmt.Sprintf("umount -f %s", mountPath)
	if _, err := utils.Run(mntCmd); err != nil {
		return err
	}
	notMnt, mntErr := utils.IsLikelyNotMountPoint(mountPath)
	if mntErr != nil {
		return err
	}
	if notMnt {
		if err := os.Remove(mountPath); err != nil {
			log.Warningf("Warning: deleting mountPath %s, with error: %s", mountPath, err.Error())
			return err
		}
		return nil
	}
	return fmt.Errorf("Failed to unmount path")
}

// Getvolumename Support
func (p *DiskPlugin) Getvolumename(opts interface{}) utils.Result {
	opt := opts.(*DiskOptions)
	return utils.Result{
		Status:     "Success",
		VolumeName: opt.VolumeName,
	}
}

// Waitforattach Not Support
func (p *DiskPlugin) Waitforattach(devicePath string, opts interface{}) utils.Result {
	opt := opts.(*DiskOptions)
	if devicePath == "" {
		utils.FinishError("Waitforattach, devicePath is empty, cannot used for Volume: " + opt.VolumeName)
	}
	if !utils.IsFileExisting(devicePath) {
		utils.FinishError("Waitforattach, devicePath: " + devicePath + " is not exist, cannot used for Volume: " + opt.VolumeName)
	}

	// check the device is used for system
	if devicePath == "/dev/vda" || devicePath == "/dev/vda1" {
		utils.FinishError("Waitforattach, devicePath: " + devicePath + " is system device, cannot used for Volume: " + opt.VolumeName)
	}
	if devicePath == "/dev/vdb1" {
		checkCmd := fmt.Sprintf("mount | grep \"/dev/vdb1 on /var/lib/kubelet type\" | wc -l")
		if out, err := utils.Run(checkCmd); err != nil {
			utils.FinishError("Waitforattach, devicePath: " + devicePath + " is check vdb error for Volume: " + opt.VolumeName)
		} else if strings.TrimSpace(out) != "0" {
			utils.FinishError("Waitforattach, devicePath: " + devicePath + " is used as DataDisk for kubelet,  cannot used fo Volume: " + opt.VolumeName)
		}
	}

	log.Infof("Waitforattach, wait for attach: %s, %s", devicePath, opt.VolumeName)
	return utils.Result{
		Status: "Success",
		Device: devicePath,
	}
}

// Mountdevice Not Support
func (p *DiskPlugin) Mountdevice(mountPath string, opts interface{}) utils.Result {
	return utils.NotSupport()
}

//
func (p *DiskPlugin) initEcsClient() {
	accessKeyID, accessSecret, accessToken, ecsEndpoint := "", "", "", ""
	// Apsara Stack use local config file
	accessKeyID, accessSecret, ecsEndpoint = p.GetDiskLocalConfig()

	// the common environment
	if accessKeyID == "" || accessSecret == "" {
		accessKeyID, accessSecret, accessToken = utils.GetDefaultAK()
	}

	p.client = newEcsClient(accessKeyID, accessSecret, accessToken, ecsEndpoint)
	if p.client == nil {
		utils.FinishError("New Ecs Client error, ak_id: " + accessKeyID)
	}
}

// GetDiskLocalConfig read disk config from local file
func (p *DiskPlugin) GetDiskLocalConfig() (string, string, string) {
	accessKeyID, accessSecret, ecsEndpoint := "", "", ""

	if utils.IsFileExisting(DISK_AKID) && utils.IsFileExisting(DISK_AKSECRET) && utils.IsFileExisting(DISK_ECSENPOINT) {
		raw, err := ioutil.ReadFile(DISK_AKID)
		if err != nil {
			log.Error("Read disk AK ID file error:", err.Error())
			return "", "", ""
		}
		accessKeyID = string(raw)

		raw, err = ioutil.ReadFile(DISK_AKSECRET)
		if err != nil {
			log.Error("Read disk AK Secret file error:", err.Error())
			return "", "", ""
		}
		accessSecret = string(raw)

		raw, err = ioutil.ReadFile(DISK_ECSENPOINT)
		if err != nil {
			log.Error("Read disk ecs Endpoint file error:", err.Error())
			return "", "", ""
		}
		ecsEndpoint = string(raw)
	}
	return strings.TrimSpace(accessKeyID), strings.TrimSpace(accessSecret), strings.TrimSpace(ecsEndpoint)
}

func getDevicePath(before, after []string) []string {
	var devicePaths []string
	for _, d := range after {
		var isNew = true
		for _, a := range before {
			if d == a {
				isNew = false
			}
		}
		if isNew {
			devicePaths = append(devicePaths, d)
		}
	}
	return devicePaths
}

// endpoint: env variable first; /etc/.volumeak/diskEcsEndpoint second, overseas region third;
func newEcsClient(accessKeyId, accessKeySecret, accessToken, ecsEndpoint string) *ecs.Client {
	m := metadata.NewMetaData(nil)
	region, err := m.Region()
	if err != nil {
		region = string(DEFAULT_REGION)
	}

	// use environment endpoint first;
	if ep := os.Getenv("ECS_ENDPOINT"); ep != "" {
		ecsEndpoint = ep
	}

	client := ecs.NewECSClientWithEndpointAndSecurityToken(ecsEndpoint, accessKeyId, accessKeySecret, accessToken, common.Region(region))
	client.SetUserAgent(KUBERNETES_ALICLOUD_IDENTITY)

	return client
}

// get diskID
func getVolumeConfig(volumeName string) string {
	volumeFile := path.Join(VolumeDir, volumeName+".conf")
	if !utils.IsFileExisting(volumeFile) {
		return ""
	}

	value, err := ioutil.ReadFile(volumeFile)
	if err != nil {
		return ""
	}

	volumeId := strings.TrimSpace(string(value))
	return volumeId
}

// save diskID and volume name
func saveVolumeConfig(opt *DiskOptions) error {
	if err := utils.CreateDest(VolumeDir); err != nil {
		return err
	}
	if err := utils.CreateDest(VolumeDirRemove); err != nil {
		return err
	}
	if err := removeVolumeConfig(opt.VolumeName); err != nil {
		return err
	}

	volumeFile := path.Join(VolumeDir, opt.VolumeName+".conf")
	return ioutil.WriteFile(volumeFile, []byte(opt.VolumeId), 0644)
}

// move config file to remove dir
func removeVolumeConfig(volumeName string) error {
	volumeFile := path.Join(VolumeDir, volumeName+".conf")
	if utils.IsFileExisting(volumeFile) {
		timeStr := time.Now().Format("2006-01-02-15:04:05")
		removeFile := path.Join(VolumeDirRemove, volumeName+"-"+timeStr+".conf")
		if err := os.Rename(volumeFile, removeFile); err != nil {
			return err
		}
	}
	return nil
}
