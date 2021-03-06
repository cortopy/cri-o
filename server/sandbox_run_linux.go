// +build linux

package server

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/current"
	"github.com/containers/libpod/pkg/annotations"
	"github.com/containers/libpod/pkg/cgroups"
	"github.com/containers/storage"
	"github.com/cri-o/cri-o/internal/lib"
	libsandbox "github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	oci "github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/pkg/sandbox"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runc/libcontainer/cgroups/systemd"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/kubelet/leaky"
	"k8s.io/kubernetes/pkg/kubelet/types"
)

const cgroupMemorySubsystemMountPathV1 = "/sys/fs/cgroup/memory"
const cgroupMemorySubsystemMountPathV2 = "/sys/fs/cgroup"

func (s *Server) runPodSandbox(ctx context.Context, req *pb.RunPodSandboxRequest) (resp *pb.RunPodSandboxResponse, err error) {
	s.updateLock.RLock()
	defer s.updateLock.RUnlock()

	sbox := sandbox.New(ctx)
	if err := sbox.SetConfig(req.GetConfig()); err != nil {
		return nil, errors.Wrap(err, "setting sandbox config")
	}

	pathsToChown := []string{}

	// we need to fill in the container name, as it is not present in the request. Luckily, it is a constant.
	log.Infof(ctx, "attempting to run pod sandbox with infra container: %s%s", translateLabelsToDescription(sbox.Config().GetLabels()), leaky.PodInfraContainerName)

	kubeName := sbox.Config().GetMetadata().GetName()
	namespace := sbox.Config().GetMetadata().GetNamespace()
	attempt := sbox.Config().GetMetadata().GetAttempt()

	id, name, err := s.ReservePodIDAndName(sbox.Config())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			s.ReleasePodName(name)
		}
	}()

	containerName, err := s.ReserveSandboxContainerIDAndName(sbox.Config())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			s.ReleaseContainerName(containerName)
		}
	}()

	var labelOptions []string
	securityContext := sbox.Config().GetLinux().GetSecurityContext()
	selinuxConfig := securityContext.GetSelinuxOptions()
	if selinuxConfig != nil {
		labelOptions = getLabelOptions(selinuxConfig)
	}
	podContainer, err := s.StorageRuntimeServer().CreatePodSandbox(s.config.SystemContext,
		name, id,
		s.config.PauseImage,
		s.config.PauseImageAuthFile,
		"",
		containerName,
		kubeName,
		sbox.Config().GetMetadata().GetUid(),
		namespace,
		attempt,
		s.defaultIDMappings,
		labelOptions)

	mountLabel := podContainer.MountLabel
	processLabel := podContainer.ProcessLabel

	if errors.Cause(err) == storage.ErrDuplicateName {
		return nil, fmt.Errorf("pod sandbox with name %q already exists", name)
	}
	if err != nil {
		return nil, fmt.Errorf("error creating pod sandbox with name %q: %v", name, err)
	}
	defer func() {
		if err != nil {
			if err2 := s.StorageRuntimeServer().RemovePodSandbox(id); err2 != nil {
				log.Warnf(ctx, "couldn't cleanup pod sandbox %q: %v", id, err2)
			}
		}
	}()

	// TODO: factor generating/updating the spec into something other projects can vendor

	// creates a spec Generator with the default spec.
	g, err := generate.New("linux")
	if err != nil {
		return nil, err
	}
	g.HostSpecific = true
	g.ClearProcessRlimits()

	ulimits, err := getUlimitsFromConfig(&s.config)
	if err != nil {
		return nil, err
	}
	for _, u := range ulimits {
		g.AddProcessRlimits(u.name, u.hard, u.soft)
	}

	// setup defaults for the pod sandbox
	g.SetRootReadonly(true)

	pauseCommand, err := PauseCommand(s.Config(), podContainer.Config)
	if err != nil {
		return nil, err
	}
	g.SetProcessArgs(pauseCommand)

	// set DNS options
	var resolvPath string
	if sbox.Config().GetDnsConfig() != nil {
		dnsServers := sbox.Config().GetDnsConfig().Servers
		dnsSearches := sbox.Config().GetDnsConfig().Searches
		dnsOptions := sbox.Config().GetDnsConfig().Options
		resolvPath = fmt.Sprintf("%s/resolv.conf", podContainer.RunDir)
		err = parseDNSOptions(dnsServers, dnsSearches, dnsOptions, resolvPath)
		if err != nil {
			err1 := removeFile(resolvPath)
			if err1 != nil {
				err = err1
				return nil, fmt.Errorf("%v; failed to remove %s: %v", err, resolvPath, err1)
			}
			return nil, err
		}
		if err := label.Relabel(resolvPath, mountLabel, false); err != nil && errors.Cause(err) != unix.ENOTSUP {
			return nil, err
		}
		mnt := spec.Mount{
			Type:        "bind",
			Source:      resolvPath,
			Destination: "/etc/resolv.conf",
			Options:     []string{"ro", "bind", "nodev", "nosuid", "noexec"},
		}
		pathsToChown = append(pathsToChown, resolvPath)
		g.AddMount(mnt)
	}

	// add metadata
	metadata := sbox.Config().GetMetadata()
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}

	// add labels
	labels := sbox.Config().GetLabels()

	if err := validateLabels(labels); err != nil {
		return nil, err
	}

	// Add special container name label for the infra container
	if labels != nil {
		labels[types.KubernetesContainerNameLabel] = leaky.PodInfraContainerName
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return nil, err
	}

	// add annotations
	kubeAnnotations := sbox.Config().GetAnnotations()
	kubeAnnotationsJSON, err := json.Marshal(kubeAnnotations)
	if err != nil {
		return nil, err
	}

	// set log directory
	logDir := sbox.Config().GetLogDirectory()
	if logDir == "" {
		logDir = filepath.Join(s.config.LogDir, id)
	}
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return nil, err
	}
	// This should always be absolute from k8s.
	if !filepath.IsAbs(logDir) {
		return nil, fmt.Errorf("requested logDir for sbox id %s is a relative path: %s", id, logDir)
	}

	privileged := s.privilegedSandbox(req)

	// Add capabilities from crio.conf if default_capabilities is defined
	capabilities := &pb.Capability{}
	if s.config.DefaultCapabilities != nil {
		g.ClearProcessCapabilities()
		capabilities.AddCapabilities = append(capabilities.AddCapabilities, s.config.DefaultCapabilities...)
	}
	if err := setupCapabilities(&g, capabilities); err != nil {
		return nil, err
	}

	nsOptsJSON, err := json.Marshal(securityContext.GetNamespaceOptions())
	if err != nil {
		return nil, err
	}

	hostIPC := securityContext.GetNamespaceOptions().GetIpc() == pb.NamespaceMode_NODE
	hostPID := securityContext.GetNamespaceOptions().GetPid() == pb.NamespaceMode_NODE

	// Don't use SELinux separation with Host Pid or IPC Namespace or privileged.
	if hostPID || hostIPC {
		processLabel, mountLabel = "", ""
	}
	g.SetProcessSelinuxLabel(processLabel)
	g.SetLinuxMountLabel(mountLabel)

	// Remove the default /dev/shm mount to ensure we overwrite it
	g.RemoveMount(libsandbox.DevShmPath)

	// create shm mount for the pod containers.
	var shmPath string
	if hostIPC {
		shmPath = libsandbox.DevShmPath
	} else {
		shmPath, err = setupShm(podContainer.RunDir, mountLabel)
		if err != nil {
			return nil, err
		}
		pathsToChown = append(pathsToChown, shmPath)
		defer func() {
			if err != nil {
				if err2 := unix.Unmount(shmPath, unix.MNT_DETACH); err2 != nil {
					log.Warnf(ctx, "failed to unmount shm for pod: %v", err2)
				}
			}
		}()
	}

	mnt := spec.Mount{
		Type:        "bind",
		Source:      shmPath,
		Destination: libsandbox.DevShmPath,
		Options:     []string{"rw", "bind"},
	}
	// bind mount the pod shm
	g.AddMount(mnt)

	err = s.setPodSandboxMountLabel(id, mountLabel)
	if err != nil {
		return nil, err
	}

	if err := s.CtrIDIndex().Add(id); err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			if err2 := s.CtrIDIndex().Delete(id); err2 != nil {
				log.Warnf(ctx, "couldn't delete ctr id %s from idIndex", id)
			}
		}
	}()

	// set log path inside log directory
	logPath := filepath.Join(logDir, id+".log")

	// Handle https://issues.k8s.io/44043
	if err := ensureSaneLogPath(logPath); err != nil {
		return nil, err
	}

	hostNetwork := securityContext.GetNamespaceOptions().GetNetwork() == pb.NamespaceMode_NODE

	hostname, err := getHostname(id, sbox.Config().Hostname, hostNetwork)
	if err != nil {
		return nil, err
	}
	g.SetHostname(hostname)

	// validate the runtime handler
	runtimeHandler, err := s.runtimeHandler(req)
	if err != nil {
		return nil, err
	}

	g.AddAnnotation(annotations.Metadata, string(metadataJSON))
	g.AddAnnotation(annotations.Labels, string(labelsJSON))
	g.AddAnnotation(annotations.Annotations, string(kubeAnnotationsJSON))
	g.AddAnnotation(annotations.LogPath, logPath)
	g.AddAnnotation(annotations.Name, name)
	g.AddAnnotation(annotations.Namespace, namespace)
	g.AddAnnotation(annotations.ContainerType, annotations.ContainerTypeSandbox)
	g.AddAnnotation(annotations.SandboxID, id)
	g.AddAnnotation(annotations.ContainerName, containerName)
	g.AddAnnotation(annotations.ContainerID, id)
	g.AddAnnotation(annotations.ShmPath, shmPath)
	g.AddAnnotation(annotations.PrivilegedRuntime, fmt.Sprintf("%v", privileged))
	g.AddAnnotation(annotations.RuntimeHandler, runtimeHandler)
	g.AddAnnotation(annotations.ResolvPath, resolvPath)
	g.AddAnnotation(annotations.HostName, hostname)
	g.AddAnnotation(annotations.NamespaceOptions, string(nsOptsJSON))
	g.AddAnnotation(annotations.KubeName, kubeName)
	g.AddAnnotation(annotations.HostNetwork, fmt.Sprintf("%v", hostNetwork))
	g.AddAnnotation(annotations.ContainerManager, lib.ContainerManagerCRIO)
	if podContainer.Config.Config.StopSignal != "" {
		// this key is defined in image-spec conversion document at https://github.com/opencontainers/image-spec/pull/492/files#diff-8aafbe2c3690162540381b8cdb157112R57
		g.AddAnnotation("org.opencontainers.image.stopSignal", podContainer.Config.Config.StopSignal)
	}

	created := time.Now()
	g.AddAnnotation(annotations.Created, created.Format(time.RFC3339Nano))

	portMappings := convertPortMappings(sbox.Config().GetPortMappings())
	portMappingsJSON, err := json.Marshal(portMappings)
	if err != nil {
		return nil, err
	}
	g.AddAnnotation(annotations.PortMappings, string(portMappingsJSON))

	parent := ""
	cgroupv2, err := cgroups.IsCgroup2UnifiedMode()
	if err != nil {
		return nil, err
	}
	if cgroupv2 {
		parent = cgroupMemorySubsystemMountPathV2
	} else {
		parent = cgroupMemorySubsystemMountPathV1
	}

	cgroupParent, err := AddCgroupAnnotation(ctx, g, parent, s.config.CgroupManager, sbox.Config().GetLinux().GetCgroupParent(), id)
	if err != nil {
		return nil, err
	}

	if s.defaultIDMappings != nil && !s.defaultIDMappings.Empty() {
		if err := g.AddOrReplaceLinuxNamespace(string(spec.UserNamespace), ""); err != nil {
			return nil, errors.Wrap(err, "add or replace linux namespace")
		}
		for _, uidmap := range s.defaultIDMappings.UIDs() {
			g.AddLinuxUIDMapping(uint32(uidmap.HostID), uint32(uidmap.ContainerID), uint32(uidmap.Size))
		}
		for _, gidmap := range s.defaultIDMappings.GIDs() {
			g.AddLinuxGIDMapping(uint32(gidmap.HostID), uint32(gidmap.ContainerID), uint32(gidmap.Size))
		}
	}

	sb, err := libsandbox.New(id, namespace, name, kubeName, logDir, labels, kubeAnnotations, processLabel, mountLabel, metadata, shmPath, cgroupParent, privileged, runtimeHandler, resolvPath, hostname, portMappings, hostNetwork)
	if err != nil {
		return nil, err
	}

	if err := s.addSandbox(sb); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			if err := s.removeSandbox(id); err != nil {
				log.Warnf(ctx, "could not remove pod sandbox: %v", err)
			}
		}
	}()

	if err := s.PodIDIndex().Add(id); err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			if err := s.PodIDIndex().Delete(id); err != nil {
				log.Warnf(ctx, "couldn't delete pod id %s from idIndex", id)
			}
		}
	}()

	for k, v := range kubeAnnotations {
		g.AddAnnotation(k, v)
	}
	for k, v := range labels {
		g.AddAnnotation(k, v)
	}

	// Add default sysctls given in crio.conf
	s.configureGeneratorForSysctls(ctx, g, hostNetwork, hostIPC)
	// extract linux sysctls from annotations and pass down to oci runtime
	// Will override any duplicate default systcl from crio.conf
	for key, value := range sbox.Config().GetLinux().GetSysctls() {
		g.AddLinuxSysctl(key, value)
	}

	// Set OOM score adjust of the infra container to be very low
	// so it doesn't get killed.
	g.SetProcessOOMScoreAdj(PodInfraOOMAdj)

	g.SetLinuxResourcesCPUShares(PodInfraCPUshares)

	// set up namespaces
	cleanupFuncs, err := s.configureGeneratorForSandboxNamespaces(hostNetwork, hostIPC, hostPID, sb, g)
	// We want to cleanup after ourselves if we are managing any namespaces and fail in this function.
	for idx := range cleanupFuncs {
		defer func(currentFunc int) {
			if err != nil {
				if err2 := cleanupFuncs[currentFunc](); err2 != nil {
					log.Debugf(ctx, err2.Error())
				}
			}
		}(idx)
	}
	if err != nil {
		return nil, err
	}

	if s.Config().Seccomp().IsDisabled() {
		g.Config.Linux.Seccomp = nil
	}

	saveOptions := generate.ExportOptions{}
	mountPoint, err := s.StorageRuntimeServer().StartContainer(id)
	if err != nil {
		return nil, fmt.Errorf("failed to mount container %s in pod sandbox %s(%s): %v", containerName, sb.Name(), id, err)
	}
	g.AddAnnotation(annotations.MountPoint, mountPoint)

	hostnamePath := fmt.Sprintf("%s/hostname", podContainer.RunDir)
	if err := ioutil.WriteFile(hostnamePath, []byte(hostname+"\n"), 0644); err != nil {
		return nil, err
	}
	if err := label.Relabel(hostnamePath, mountLabel, false); err != nil && errors.Cause(err) != unix.ENOTSUP {
		return nil, err
	}
	mnt = spec.Mount{
		Type:        "bind",
		Source:      hostnamePath,
		Destination: "/etc/hostname",
		Options:     []string{"ro", "bind", "nodev", "nosuid", "noexec"},
	}
	pathsToChown = append(pathsToChown, hostnamePath)
	g.AddMount(mnt)
	g.AddAnnotation(annotations.HostnamePath, hostnamePath)
	sb.AddHostnamePath(hostnamePath)

	container, err := oci.NewContainer(id, containerName, podContainer.RunDir, logPath, labels, g.Config.Annotations, kubeAnnotations, "", "", "", nil, id, false, false, false, sb.Privileged(), sb.RuntimeHandler(), podContainer.Dir, created, podContainer.Config.Config.StopSignal)
	if err != nil {
		return nil, err
	}
	container.SetMountPoint(mountPoint)

	container.SetIDMappings(s.defaultIDMappings)

	if s.defaultIDMappings != nil && !s.defaultIDMappings.Empty() {
		if securityContext.GetNamespaceOptions().GetIpc() == pb.NamespaceMode_NODE {
			g.RemoveMount("/dev/mqueue")
			mqueue := spec.Mount{
				Type:        "bind",
				Source:      "/dev/mqueue",
				Destination: "/dev/mqueue",
				Options:     []string{"rw", "rbind", "nodev", "nosuid", "noexec"},
			}
			g.AddMount(mqueue)
		}
		if hostNetwork {
			g.RemoveMount("/sys")
			g.RemoveMount("/sys/cgroup")
			sysMnt := spec.Mount{
				Destination: "/sys",
				Type:        "bind",
				Source:      "/sys",
				Options:     []string{"nosuid", "noexec", "nodev", "ro", "rbind"},
			}
			g.AddMount(sysMnt)
		}
		if securityContext.GetNamespaceOptions().GetPid() == pb.NamespaceMode_NODE {
			g.RemoveMount("/proc")
			proc := spec.Mount{
				Type:        "bind",
				Source:      "/proc",
				Destination: "/proc",
				Options:     []string{"rw", "rbind", "nodev", "nosuid", "noexec"},
			}
			g.AddMount(proc)
		}
	}
	g.SetRootPath(mountPoint)

	if os.Getenv("_CRIO_ROOTLESS") != "" {
		makeOCIConfigurationRootless(&g)
	}

	container.SetSpec(g.Config)

	if err := sb.SetInfraContainer(container); err != nil {
		return nil, err
	}

	var ips []string
	var result cnitypes.Result

	if s.config.ManageNSLifecycle {
		ips, result, err = s.networkStart(ctx, sb)
		if err != nil {
			return nil, err
		}
		if result != nil {
			resultCurrent, err := current.NewResultFromResult(result)
			if err != nil {
				return nil, err
			}
			cniResultJSON, err := json.Marshal(resultCurrent)
			if err != nil {
				return nil, err
			}
			g.AddAnnotation(annotations.CNIResult, string(cniResultJSON))
		}
		defer func() {
			if err != nil {
				if err2 := s.networkStop(ctx, sb); err2 != nil {
					log.Errorf(ctx, "error stopping network on cleanup: %v", err2)
				}
			}
		}()
	}

	for idx, ip := range ips {
		g.AddAnnotation(fmt.Sprintf("%s.%d", annotations.IP, idx), ip)
	}
	sb.AddIPs(ips)
	sb.SetNamespaceOptions(securityContext.GetNamespaceOptions())

	spp := securityContext.GetSeccompProfilePath()
	g.AddAnnotation(annotations.SeccompProfilePath, spp)
	sb.SetSeccompProfilePath(spp)
	if !privileged {
		if err := s.setupSeccomp(ctx, &g, spp); err != nil {
			return nil, err
		}
	}

	err = g.SaveToFile(filepath.Join(podContainer.Dir, "config.json"), saveOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to save template configuration for pod sandbox %s(%s): %v", sb.Name(), id, err)
	}
	if err = g.SaveToFile(filepath.Join(podContainer.RunDir, "config.json"), saveOptions); err != nil {
		return nil, fmt.Errorf("failed to write runtime configuration for pod sandbox %s(%s): %v", sb.Name(), id, err)
	}

	s.addInfraContainer(container)
	defer func() {
		if err != nil {
			s.removeInfraContainer(container)
		}
	}()

	if s.defaultIDMappings != nil && !s.defaultIDMappings.Empty() {
		rootPair := s.defaultIDMappings.RootPair()
		for _, path := range pathsToChown {
			if err := os.Chown(path, rootPair.UID, rootPair.GID); err != nil {
				return nil, errors.Wrapf(err, "cannot chown %s to %d:%d", path, rootPair.UID, rootPair.GID)
			}
		}
	}

	if err := s.createContainerPlatform(container, sb.CgroupParent()); err != nil {
		return nil, err
	}

	if err := s.Runtime().StartContainer(container); err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			// Clean-up steps from RemovePodSanbox
			timeout := int64(10)
			if err2 := s.Runtime().StopContainer(ctx, container, timeout); err2 != nil {
				log.Warnf(ctx, "failed to stop container %s: %v", container.Name(), err2)
			}
			if err2 := s.Runtime().WaitContainerStateStopped(ctx, container); err2 != nil {
				log.Warnf(ctx, "failed to get container 'stopped' status %s in pod sandbox %s: %v", container.Name(), sb.ID(), err2)
			}
			if err2 := s.Runtime().DeleteContainer(container); err2 != nil {
				log.Warnf(ctx, "failed to delete container %s in pod sandbox %s: %v", container.Name(), sb.ID(), err2)
			}
			if err2 := s.ContainerStateToDisk(container); err2 != nil {
				log.Warnf(ctx, "failed to write container state %s in pod sandbox %s: %v", container.Name(), sb.ID(), err2)
			}
		}
	}()

	if err := s.ContainerStateToDisk(container); err != nil {
		log.Warnf(ctx, "unable to write containers %s state to disk: %v", container.ID(), err)
	}

	if !s.config.ManageNSLifecycle {
		ips, _, err = s.networkStart(ctx, sb)
		if err != nil {
			return nil, err
		}
		defer func() {
			if err != nil {
				if err2 := s.networkStop(ctx, sb); err2 != nil {
					log.Errorf(ctx, "error stopping network on cleanup: %v", err2)
				}
			}
		}()
	}
	sb.AddIPs(ips)

	sb.SetCreated()

	log.Infof(ctx, "ran pod sandbox %s with infra container: %s", container.ID(), container.Description())
	resp = &pb.RunPodSandboxResponse{PodSandboxId: id}
	return resp, nil
}

func setupShm(podSandboxRunDir, mountLabel string) (shmPath string, err error) {
	shmPath = filepath.Join(podSandboxRunDir, "shm")
	if err := os.Mkdir(shmPath, 0700); err != nil {
		return "", err
	}
	shmOptions := "mode=1777,size=" + strconv.Itoa(libsandbox.DefaultShmSize)
	if err = unix.Mount("shm", shmPath, "tmpfs", unix.MS_NOEXEC|unix.MS_NOSUID|unix.MS_NODEV,
		label.FormatMountLabel(shmOptions, mountLabel)); err != nil {
		return "", fmt.Errorf("failed to mount shm tmpfs for pod: %v", err)
	}
	return shmPath, nil
}

func AddCgroupAnnotation(ctx context.Context, g generate.Generator, mountPath, cgroupManager, cgroupParent, id string) (string, error) {
	if cgroupParent != "" {
		if cgroupManager == oci.SystemdCgroupsManager {
			if len(cgroupParent) <= 6 || !strings.HasSuffix(path.Base(cgroupParent), ".slice") {
				return "", fmt.Errorf("cri-o configured with systemd cgroup manager, but did not receive slice as parent: %s", cgroupParent)
			}
			cgPath := convertCgroupFsNameToSystemd(cgroupParent)
			g.SetLinuxCgroupsPath(cgPath + ":" + "crio" + ":" + id)
			cgroupParent = cgPath

			// check memory limit is greater than the minimum memory limit of 4Mb
			// expand the cgroup slice path
			slicePath, err := systemd.ExpandSlice(cgroupParent)
			if err != nil {
				return "", errors.Wrapf(err, "error expanding systemd slice path for %q", cgroupParent)
			}
			filename := ""
			cgroupv2, err := cgroups.IsCgroup2UnifiedMode()
			if err != nil {
				return "", err
			}
			if cgroupv2 {
				filename = "memory.max"
			} else {
				filename = "memory.limit_in_bytes"
			}

			// read in the memory limit from the memory.limit_in_bytes file
			fileData, err := ioutil.ReadFile(filepath.Join(mountPath, slicePath, filename))
			if err != nil {
				if os.IsNotExist(err) {
					log.Warnf(ctx, "Failed to find %s for slice: %q", filename, cgroupParent)
				} else {
					return "", errors.Wrapf(err, "error reading %s file for slice %q", filename, cgroupParent)
				}
			} else {
				// strip off the newline character and convert it to an int
				strMemory := strings.TrimRight(string(fileData), "\n")
				if strMemory != "" && strMemory != "max" {
					memoryLimit, err := strconv.ParseInt(strMemory, 10, 64)
					if err != nil {
						return "", errors.Wrapf(err, "error converting cgroup memory value from string to int %q", strMemory)
					}
					// Compare with the minimum allowed memory limit
					if memoryLimit != 0 && memoryLimit < minMemoryLimit {
						return "", fmt.Errorf("pod set memory limit %v too low; should be at least %v", memoryLimit, minMemoryLimit)
					}
				}
			}
		} else {
			if strings.HasSuffix(path.Base(cgroupParent), ".slice") {
				return "", fmt.Errorf("cri-o configured with cgroupfs cgroup manager, but received systemd slice as parent: %s", cgroupParent)
			}
			cgPath := filepath.Join(cgroupParent, scopePrefix+"-"+id)
			g.SetLinuxCgroupsPath(cgPath)
		}
	}
	g.AddAnnotation(annotations.CgroupParent, cgroupParent)

	return cgroupParent, nil
}

// PauseCommand returns the pause command for the provided image configuration.
func PauseCommand(cfg *config.Config, image *v1.Image) ([]string, error) {
	if cfg == nil {
		return nil, fmt.Errorf("provided configuration is nil")
	}

	// This has been explicitly set by the user, since the configuration
	// default is `/pause`
	if cfg.PauseCommand == "" {
		if image == nil ||
			(len(image.Config.Entrypoint) == 0 && len(image.Config.Cmd) == 0) {
			return nil, fmt.Errorf(
				"unable to run pause image %q: %s",
				cfg.PauseImage,
				"neither Cmd nor Entrypoint specified",
			)
		}
		cmd := []string{}
		cmd = append(cmd, image.Config.Entrypoint...)
		cmd = append(cmd, image.Config.Cmd...)
		return cmd, nil
	}
	return []string{cfg.PauseCommand}, nil
}

func (s *Server) configureGeneratorForSysctls(ctx context.Context, g generate.Generator, hostNetwork, hostIPC bool) {
	sysctls, err := s.config.RuntimeConfig.Sysctls()
	if err != nil {
		log.Warnf(ctx, "sysctls invalid: %v", err)
	}

	for _, sysctl := range sysctls {
		if err := sysctl.Validate(hostNetwork, hostIPC); err != nil {
			log.Warnf(ctx, "skipping invalid sysctl %s: %v", sysctl, err)
			continue
		}
		g.AddLinuxSysctl(sysctl.Key(), sysctl.Value())
	}
}

// configureGeneratorForSandboxNamespaces set the linux namespaces for the generator, based on whether the pod is sharing namespaces with the host,
// as well as whether CRI-O should be managing the namespace lifecycle.
// it returns a slice of cleanup funcs, all of which are the respective NamespaceRemove() for the sandbox.
// The caller should defer the cleanup funcs if there is an error, to make sure each namespace we are managing is properly cleaned up.
func (s *Server) configureGeneratorForSandboxNamespaces(hostNetwork, hostIPC, hostPID bool, sb *libsandbox.Sandbox, g generate.Generator) (cleanupFuncs []func() error, err error) {
	managedNamespaces := make([]libsandbox.NSType, 0, 3)
	if hostNetwork {
		err = g.RemoveLinuxNamespace(string(spec.NetworkNamespace))
		if err != nil {
			return
		}
	} else if s.config.ManageNSLifecycle {
		managedNamespaces = append(managedNamespaces, libsandbox.NETNS)
	}

	if hostIPC {
		err = g.RemoveLinuxNamespace(string(spec.IPCNamespace))
		if err != nil {
			return
		}
	} else if s.config.ManageNSLifecycle {
		managedNamespaces = append(managedNamespaces, libsandbox.IPCNS)
	}

	// Since we need a process to hold open the PID namespace, CRI-O can't manage the NS lifecycle
	if hostPID {
		err = g.RemoveLinuxNamespace(string(spec.PIDNamespace))
		if err != nil {
			return
		}
	}

	// There's no option to set hostUTS
	if s.config.ManageNSLifecycle {
		managedNamespaces = append(managedNamespaces, libsandbox.UTSNS)

		// now that we've configured the namespaces we're sharing, tell sandbox to configure them
		managedNamespaces, err := sb.CreateManagedNamespaces(managedNamespaces, &s.config)
		if err != nil {
			return nil, err
		}

		cleanupFuncs = append(cleanupFuncs, sb.RemoveManagedNamespaces)

		if err := configureGeneratorGivenNamespacePaths(managedNamespaces, g); err != nil {
			return cleanupFuncs, err
		}
	}

	return cleanupFuncs, err
}

// configureGeneratorGivenNamespacePaths takes a map of nsType -> nsPath. It configures the generator
// to add or replace the defaults to these paths
func configureGeneratorGivenNamespacePaths(managedNamespaces []*libsandbox.ManagedNamespace, g generate.Generator) error {
	typeToSpec := map[libsandbox.NSType]spec.LinuxNamespaceType{
		libsandbox.IPCNS:  spec.IPCNamespace,
		libsandbox.NETNS:  spec.NetworkNamespace,
		libsandbox.UTSNS:  spec.UTSNamespace,
		libsandbox.USERNS: spec.UserNamespace,
	}

	for _, ns := range managedNamespaces {
		// allow for empty paths, as this namespace just shouldn't be configured
		if ns.Path() == "" {
			continue
		}
		nsForSpec := typeToSpec[ns.Type()]
		if nsForSpec == "" {
			return errors.Errorf("Invalid namespace type %s", nsForSpec)
		}
		err := g.AddOrReplaceLinuxNamespace(string(nsForSpec), ns.Path())
		if err != nil {
			return err
		}
	}
	return nil
}
