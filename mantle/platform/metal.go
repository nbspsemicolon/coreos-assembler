// Copyright 2020 Red Hat
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

package platform

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	coreosarch "github.com/coreos/stream-metadata-go/arch"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"

	"github.com/coreos/coreos-assembler/mantle/platform/conf"
	"github.com/coreos/coreos-assembler/mantle/system/exec"
	"github.com/coreos/coreos-assembler/mantle/util"
)

const (
	// defaultQemuHostIPv4 is documented in `man qemu-kvm`, under the `-netdev` option
	defaultQemuHostIPv4 = "10.0.2.2"

	bootStartedSignal = "boot-started-OK"
)

// TODO derive this from docs, or perhaps include kargs in cosa metadata?
var baseKargs = []string{"rd.neednet=1", "ip=dhcp", "ignition.firstboot", "ignition.platform.id=metal"}

var (
	// TODO expose this as an API that can be used by cosa too
	consoleKernelArgument = map[string]string{
		"x86_64":  "ttyS0,115200n8",
		"ppc64le": "hvc0",
		"aarch64": "ttyAMA0",
		"s390x":   "ttysclp0",
	}

	bootStartedUnit = fmt.Sprintf(`[Unit]
	Description=TestISO Boot Started
	Requires=dev-virtio\\x2dports-bootstarted.device
	OnFailure=emergency.target
	OnFailureJobMode=isolate
	[Service]
	Type=oneshot
	RemainAfterExit=yes
	ExecStart=/bin/sh -c '/usr/bin/echo %s >/dev/virtio-ports/bootstarted'
	[Install]
	RequiredBy=coreos-installer.target
	`, bootStartedSignal)
)

// NewMetalQemuBuilderDefault returns a QEMU builder instance with some
// defaults set up for bare metal.
func NewMetalQemuBuilderDefault() *QemuBuilder {
	builder := NewQemuBuilder()
	// https://github.com/coreos/fedora-coreos-tracker/issues/388
	// https://github.com/coreos/fedora-coreos-docs/pull/46
	builder.MemoryMiB = 4096
	return builder
}

type Install struct {
	CosaBuild       *util.LocalBuild
	Builder         *QemuBuilder
	Insecure        bool
	Native4k        bool
	MultiPathDisk   bool
	PxeAppendRootfs bool
	NmKeyfiles      map[string]string

	// These are set by the install path
	kargs        []string
	ignition     conf.Conf
	liveIgnition conf.Conf
}

type InstalledMachine struct {
	Tempdir                 string
	QemuInst                *QemuInstance
	BootStartedErrorChannel chan error
}

// Check that artifact has been built and locally exists
func (inst *Install) checkArtifactsExist(artifacts []string) error {
	version := inst.CosaBuild.Meta.OstreeVersion
	for _, name := range artifacts {
		artifact, err := inst.CosaBuild.Meta.GetArtifact(name)
		if err != nil {
			return fmt.Errorf("Missing artifact %s for %s build: %s", name, version, err)
		}
		path := filepath.Join(inst.CosaBuild.Dir, artifact.Path)
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("Missing local file for artifact %s for build %s", name, version)
			}
		}
	}
	return nil
}

func (inst *Install) PXE(kargs []string, liveIgnition, ignition conf.Conf, offline bool) (*InstalledMachine, error) {
	artifacts := []string{"live-kernel", "live-rootfs"}
	if err := inst.checkArtifactsExist(artifacts); err != nil {
		return nil, err
	}

	installerConfig := installerConfig{
		Console:     []string{consoleKernelArgument[coreosarch.CurrentRpmArch()]},
		AppendKargs: renderCosaTestIsoDebugKargs(),
	}
	installerConfigData, err := yaml.Marshal(installerConfig)
	if err != nil {
		return nil, err
	}
	mode := 0644

	// XXX: https://github.com/coreos/coreos-installer/issues/1171
	if coreosarch.CurrentRpmArch() != "s390x" {
		liveIgnition.AddFile("/etc/coreos/installer.d/mantle.yaml", string(installerConfigData), mode)
	}

	inst.kargs = append(renderCosaTestIsoDebugKargs(), kargs...)
	inst.ignition = ignition
	inst.liveIgnition = liveIgnition

	mach, err := inst.runPXE(&kernelSetup{
		kernel:    inst.CosaBuild.Meta.BuildArtifacts.LiveKernel.Path,
		initramfs: inst.CosaBuild.Meta.BuildArtifacts.LiveInitramfs.Path,
		rootfs:    inst.CosaBuild.Meta.BuildArtifacts.LiveRootfs.Path,
	}, offline)
	if err != nil {
		return nil, errors.Wrapf(err, "testing live installer")
	}

	return mach, nil
}

func (inst *InstalledMachine) Destroy() error {
	if inst.QemuInst != nil {
		inst.QemuInst.Destroy()
		inst.QemuInst = nil
	}
	if inst.Tempdir != "" {
		return os.RemoveAll(inst.Tempdir)
	}
	return nil
}

type kernelSetup struct {
	kernel, initramfs, rootfs string
}

type pxeSetup struct {
	tftpipaddr    string
	boottype      string
	networkdevice string
	bootindex     string
	pxeimagepath  string

	// bootfile is initialized later
	bootfile string
}

type installerRun struct {
	inst    *Install
	builder *QemuBuilder

	builddir string
	tempdir  string
	tftpdir  string

	metalimg  string
	metalname string

	baseurl string

	kern kernelSetup
	pxe  pxeSetup
}

func absSymlink(src, dest string) error {
	src, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	return os.Symlink(src, dest)
}

// setupMetalImage creates a symlink to the metal image.
func setupMetalImage(builddir, metalimg, destdir string) (string, error) {
	if err := absSymlink(filepath.Join(builddir, metalimg), filepath.Join(destdir, metalimg)); err != nil {
		return "", err
	}
	return metalimg, nil
}

func (inst *Install) setup(kern *kernelSetup) (*installerRun, error) {
	var artifacts []string
	if inst.Native4k {
		artifacts = append(artifacts, "metal4k")
	} else {
		artifacts = append(artifacts, "metal")
	}
	if err := inst.checkArtifactsExist(artifacts); err != nil {
		return nil, err
	}

	builder := inst.Builder

	tempdir, err := os.MkdirTemp("/var/tmp", "mantle-pxe")
	if err != nil {
		return nil, err
	}
	cleanupTempdir := true
	defer func() {
		if cleanupTempdir {
			os.RemoveAll(tempdir)
		}
	}()

	tftpdir := filepath.Join(tempdir, "tftp")
	if err := os.Mkdir(tftpdir, 0777); err != nil {
		return nil, err
	}

	builddir := inst.CosaBuild.Dir
	if err := inst.ignition.WriteFile(filepath.Join(tftpdir, "config.ign")); err != nil {
		return nil, err
	}
	// This code will ensure to add an SSH key to `pxe-live.ign` config.
	inst.liveIgnition.AddAutoLogin()
	inst.liveIgnition.AddSystemdUnit("boot-started.service", bootStartedUnit, conf.Enable)
	if err := inst.liveIgnition.WriteFile(filepath.Join(tftpdir, "pxe-live.ign")); err != nil {
		return nil, err
	}

	for _, name := range []string{kern.kernel, kern.initramfs, kern.rootfs} {
		if err := absSymlink(filepath.Join(builddir, name), filepath.Join(tftpdir, name)); err != nil {
			return nil, err
		}
	}
	if inst.PxeAppendRootfs {
		// replace the initramfs symlink with a concatenation of
		// the initramfs and rootfs
		initrd := filepath.Join(tftpdir, kern.initramfs)
		if err := os.Remove(initrd); err != nil {
			return nil, err
		}
		if err := cat(initrd, filepath.Join(builddir, kern.initramfs), filepath.Join(builddir, kern.rootfs)); err != nil {
			return nil, err
		}
	}

	var metalimg string
	if inst.Native4k {
		metalimg = inst.CosaBuild.Meta.BuildArtifacts.Metal4KNative.Path
	} else {
		metalimg = inst.CosaBuild.Meta.BuildArtifacts.Metal.Path
	}
	metalname, err := setupMetalImage(builddir, metalimg, tftpdir)
	if err != nil {
		return nil, errors.Wrapf(err, "setting up metal image")
	}

	pxe := pxeSetup{}
	pxe.tftpipaddr = "192.168.76.2"
	switch coreosarch.CurrentRpmArch() {
	case "x86_64":
		pxe.networkdevice = "e1000"
		if builder.Firmware == "uefi" {
			pxe.boottype = "grub"
			pxe.bootfile = "/boot/grub2/grubx64.efi"
			pxe.pxeimagepath = "/boot/efi/EFI/fedora/grubx64.efi"
			// Choose bootindex=2. First boot the hard drive won't
			// have an OS and will fall through to bootindex 2 (net)
			pxe.bootindex = "2"
		} else {
			pxe.boottype = "pxe"
			pxe.pxeimagepath = "/usr/share/syslinux/"
		}
	case "aarch64":
		pxe.boottype = "grub"
		pxe.networkdevice = "virtio-net-pci"
		pxe.bootfile = "/boot/grub2/grubaa64.efi"
		pxe.pxeimagepath = "/boot/efi/EFI/fedora/grubaa64.efi"
		pxe.bootindex = "1"
	case "ppc64le":
		pxe.boottype = "grub"
		pxe.networkdevice = "virtio-net-pci"
		pxe.bootfile = "/boot/grub2/powerpc-ieee1275/core.elf"
	case "s390x":
		pxe.boottype = "pxe"
		pxe.networkdevice = "virtio-net-ccw"
		pxe.tftpipaddr = "10.0.2.2"
		pxe.bootindex = "1"
	default:
		return nil, fmt.Errorf("Unsupported arch %s" + coreosarch.CurrentRpmArch())
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(tftpdir)))
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	//nolint // Yeah this leaks
	go func() {
		http.Serve(listener, mux)
	}()
	baseurl := fmt.Sprintf("http://%s:%d", pxe.tftpipaddr, port)

	cleanupTempdir = false // Transfer ownership
	return &installerRun{
		inst: inst,

		builder:  builder,
		tempdir:  tempdir,
		tftpdir:  tftpdir,
		builddir: builddir,

		metalimg:  metalimg,
		metalname: metalname,

		baseurl: baseurl,

		pxe:  pxe,
		kern: *kern,
	}, nil
}

func renderBaseKargs() []string {
	return append(baseKargs, fmt.Sprintf("console=%s", consoleKernelArgument[coreosarch.CurrentRpmArch()]))
}

func renderInstallKargs(t *installerRun, offline bool) []string {
	args := []string{"coreos.inst.install_dev=/dev/vda",
		fmt.Sprintf("coreos.inst.ignition_url=%s/config.ign", t.baseurl)}
	if !offline {
		args = append(args, fmt.Sprintf("coreos.inst.image_url=%s/%s", t.baseurl, t.metalname))
	}
	// FIXME - ship signatures by default too
	if t.inst.Insecure {
		args = append(args, "coreos.inst.insecure")
	}
	return args
}

// Sometimes the logs that stream from various virtio streams can be
// incomplete because they depend on services inside the guest.
// When you are debugging earlyboot/initramfs issues this can be
// problematic. Let's add a hook here to enable more debugging.
func renderCosaTestIsoDebugKargs() []string {
	if _, ok := os.LookupEnv("COSA_TESTISO_DEBUG"); ok {
		return []string{"systemd.log_color=0", "systemd.log_level=debug",
			"systemd.journald.forward_to_console=1",
			"systemd.journald.max_level_console=debug"}
	} else {
		return []string{}
	}
}

func (t *installerRun) destroy() error {
	t.builder.Close()
	if t.tempdir != "" {
		return os.RemoveAll(t.tempdir)
	}
	return nil
}

func (t *installerRun) completePxeSetup(kargs []string) error {
	if t.kern.rootfs != "" && !t.inst.PxeAppendRootfs {
		kargs = append(kargs, fmt.Sprintf("coreos.live.rootfs_url=%s/%s", t.baseurl, t.kern.rootfs))
	}
	kargsStr := strings.Join(kargs, " ")

	switch t.pxe.boottype {
	case "pxe":
		pxeconfigdir := filepath.Join(t.tftpdir, "pxelinux.cfg")
		if err := os.Mkdir(pxeconfigdir, 0777); err != nil {
			return errors.Wrapf(err, "creating dir %s", pxeconfigdir)
		}
		pxeimages := []string{"pxelinux.0", "ldlinux.c32"}
		pxeconfig := []byte(fmt.Sprintf(`
		DEFAULT pxeboot
		TIMEOUT 20
		PROMPT 0
		LABEL pxeboot
			KERNEL %s
			APPEND initrd=%s %s
		`, t.kern.kernel, t.kern.initramfs, kargsStr))
		if coreosarch.CurrentRpmArch() == "s390x" {
			pxeconfig = []byte(kargsStr)
		}
		pxeconfig_path := filepath.Join(pxeconfigdir, "default")
		if err := os.WriteFile(pxeconfig_path, pxeconfig, 0777); err != nil {
			return errors.Wrapf(err, "writing file %s", pxeconfig_path)
		}

		// this is only for s390x where the pxe image has to be created;
		// s390 doesn't seem to have a pre-created pxe image although have to check on this
		if t.pxe.pxeimagepath == "" {
			kernelpath := filepath.Join(t.tftpdir, t.kern.kernel)
			initrdpath := filepath.Join(t.tftpdir, t.kern.initramfs)
			err := exec.Command("/usr/bin/mk-s390image", kernelpath, "-r", initrdpath,
				"-p", filepath.Join(pxeconfigdir, "default"), filepath.Join(t.tftpdir, pxeimages[0])).Run()
			if err != nil {
				return errors.Wrap(err, "running mk-s390image")
			}
		} else {
			for _, img := range pxeimages {
				srcpath := filepath.Join("/usr/share/syslinux", img)
				cp_cmd := exec.Command("/usr/lib/coreos-assembler/cp-reflink", srcpath, t.tftpdir)
				cp_cmd.Stderr = os.Stderr
				if err := cp_cmd.Run(); err != nil {
					return errors.Wrapf(err, "running cp-reflink %s %s", srcpath, t.tftpdir)
				}
			}
		}
		t.pxe.bootfile = "/" + pxeimages[0]
	case "grub":
		grub2_mknetdir_cmd := exec.Command("grub2-mknetdir", "--net-directory="+t.tftpdir)
		grub2_mknetdir_cmd.Stderr = os.Stderr
		if err := grub2_mknetdir_cmd.Run(); err != nil {
			return errors.Wrap(err, "running grub2-mknetdir")
		}
		if t.pxe.pxeimagepath != "" {
			dstpath := filepath.Join(t.tftpdir, "boot/grub2")
			cp_cmd := exec.Command("/usr/lib/coreos-assembler/cp-reflink", t.pxe.pxeimagepath, dstpath)
			cp_cmd.Stderr = os.Stderr
			if err := cp_cmd.Run(); err != nil {
				return errors.Wrapf(err, "running cp-reflink %s %s", t.pxe.pxeimagepath, dstpath)
			}
		}
		if err := os.WriteFile(filepath.Join(t.tftpdir, "boot/grub2/grub.cfg"), []byte(fmt.Sprintf(`
			default=0
			timeout=1
			menuentry "CoreOS (BIOS/UEFI)" {
				echo "Loading kernel"
				linux /%s %s
				echo "Loading initrd"
				initrd %s
			}
		`, t.kern.kernel, kargsStr, t.kern.initramfs)), 0777); err != nil {
			return errors.Wrap(err, "writing grub.cfg")
		}
	default:
		panic("Unhandled boottype " + t.pxe.boottype)
	}

	return nil
}

func switchBootOrderSignal(qinst *QemuInstance, bootstartedchan *os.File, booterrchan *chan error) {
	*booterrchan = make(chan error)
	go func() {
		err := qinst.Wait()
		// only one Wait() gets process data, so also manually check for signal
		if err == nil && qinst.Signaled() {
			err = errors.New("process killed")
		}
		if err != nil {
			*booterrchan <- errors.Wrapf(err, "QEMU unexpectedly exited while waiting for %s", bootStartedSignal)
		}
	}()
	go func() {
		r := bufio.NewReader(bootstartedchan)
		l, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// this may be from QEMU getting killed or exiting; wait a bit
				// to give a chance for .Wait() above to feed the channel with a
				// better error
				time.Sleep(1 * time.Second)
				*booterrchan <- fmt.Errorf("Got EOF from boot started channel, %s expected", bootStartedSignal)
			} else {
				*booterrchan <- errors.Wrapf(err, "reading from boot started channel")
			}
			return
		}
		line := strings.TrimSpace(l)
		// switch the boot order here, we are well into the installation process - only for aarch64 and s390x
		if line == bootStartedSignal {
			if err := qinst.SwitchBootOrder(); err != nil {
				*booterrchan <- errors.Wrapf(err, "switching boot order failed")
				return
			}
		}
		// OK!
		*booterrchan <- nil
	}()
}

func cat(outfile string, infiles ...string) error {
	out, err := os.OpenFile(outfile, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer out.Close()
	for _, infile := range infiles {
		in, err := os.Open(infile)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(out, in)
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *installerRun) run() (*QemuInstance, error) {
	builder := t.builder
	netdev := fmt.Sprintf("%s,netdev=mynet0,mac=52:54:00:12:34:56", t.pxe.networkdevice)
	if t.pxe.bootindex == "" {
		builder.Append("-boot", "once=n")
	} else {
		netdev += fmt.Sprintf(",bootindex=%s", t.pxe.bootindex)
	}
	builder.Append("-device", netdev)
	usernetdev := fmt.Sprintf("user,id=mynet0,tftp=%s,bootfile=%s", t.tftpdir, t.pxe.bootfile)
	if t.pxe.tftpipaddr != "10.0.2.2" {
		usernetdev += ",net=192.168.76.0/24,dhcpstart=192.168.76.9"
	}
	builder.Append("-netdev", usernetdev)

	inst, err := builder.Exec()
	if err != nil {
		return nil, err
	}
	return inst, nil
}

func (inst *Install) runPXE(kern *kernelSetup, offline bool) (*InstalledMachine, error) {
	t, err := inst.setup(kern)
	if err != nil {
		return nil, errors.Wrapf(err, "setting up install")
	}
	defer func() {
		err = t.destroy()
	}()

	bootStartedChan, err := inst.Builder.VirtioChannelRead("bootstarted")
	if err != nil {
		return nil, errors.Wrapf(err, "setting up bootstarted virtio-serial channel")
	}

	kargs := renderBaseKargs()
	kargs = append(kargs, inst.kargs...)
	kargs = append(kargs, fmt.Sprintf("ignition.config.url=%s/pxe-live.ign", t.baseurl))

	kargs = append(kargs, renderInstallKargs(t, offline)...)
	if err := t.completePxeSetup(kargs); err != nil {
		return nil, errors.Wrapf(err, "completing PXE setup")
	}
	qinst, err := t.run()
	if err != nil {
		return nil, errors.Wrapf(err, "running PXE install")
	}
	tempdir := t.tempdir
	t.tempdir = "" // Transfer ownership
	instmachine := InstalledMachine{
		QemuInst: qinst,
		Tempdir:  tempdir,
	}
	switchBootOrderSignal(qinst, bootStartedChan, &instmachine.BootStartedErrorChannel)
	return &instmachine, nil
}

type installerConfig struct {
	ImageURL     string   `yaml:"image-url,omitempty"`
	IgnitionFile string   `yaml:"ignition-file,omitempty"`
	Insecure     bool     `yaml:",omitempty"`
	AppendKargs  []string `yaml:"append-karg,omitempty"`
	CopyNetwork  bool     `yaml:"copy-network,omitempty"`
	DestDevice   string   `yaml:"dest-device,omitempty"`
	Console      []string `yaml:"console,omitempty"`
}

func (inst *Install) InstallViaISOEmbed(kargs []string, liveIgnition, targetIgnition conf.Conf, outdir string, offline, minimal bool) (*InstalledMachine, error) {
	artifacts := []string{"live-iso"}
	if inst.Native4k {
		artifacts = append(artifacts, "metal4k")
	} else {
		artifacts = append(artifacts, "metal")
	}
	if err := inst.checkArtifactsExist(artifacts); err != nil {
		return nil, err
	}
	if minimal && offline { // ideally this'd be one enum parameter
		panic("Can't run minimal install offline")
	}
	if offline && len(inst.NmKeyfiles) > 0 {
		return nil, fmt.Errorf("Cannot use `--add-nm-keyfile` with offline mode")
	}

	installerConfig := installerConfig{
		IgnitionFile: "/var/opt/pointer.ign",
		DestDevice:   "/dev/vda",
		AppendKargs:  renderCosaTestIsoDebugKargs(),
	}

	// XXX: https://github.com/coreos/coreos-installer/issues/1171
	if coreosarch.CurrentRpmArch() != "s390x" {
		installerConfig.Console = []string{consoleKernelArgument[coreosarch.CurrentRpmArch()]}
	}

	if inst.MultiPathDisk {
		// we only have one multipath device so it has to be that
		installerConfig.DestDevice = "/dev/mapper/mpatha"
		installerConfig.AppendKargs = append(installerConfig.AppendKargs, "rd.multipath=default", "root=/dev/disk/by-label/dm-mpath-root", "rw")
	}

	inst.kargs = append(renderCosaTestIsoDebugKargs(), kargs...)
	inst.ignition = targetIgnition
	inst.liveIgnition = liveIgnition

	tempdir, err := os.MkdirTemp("/var/tmp", "mantle-metal")
	if err != nil {
		return nil, err
	}
	cleanupTempdir := true
	defer func() {
		if cleanupTempdir {
			os.RemoveAll(tempdir)
		}
	}()

	if err := inst.ignition.WriteFile(filepath.Join(tempdir, "target.ign")); err != nil {
		return nil, err
	}
	// and write it once more in the output dir for debugging
	if err := inst.ignition.WriteFile(filepath.Join(outdir, "config-target.ign")); err != nil {
		return nil, err
	}

	builddir := inst.CosaBuild.Dir
	srcisopath := filepath.Join(builddir, inst.CosaBuild.Meta.BuildArtifacts.LiveIso.Path)

	// Copy the ISO to a new location for modification.
	// This is a bit awkward; we copy here, but QemuBuilder will also copy
	// again (in `setupIso()`). I didn't want to lower the NM keyfile stuff
	// into QemuBuilder. And plus, both tempdirs should be in /var/tmp so
	// the `cp --reflink=auto` that QemuBuilder does should just reflink.
	newIso := filepath.Join(tempdir, "install.iso")
	cmd := exec.Command("cp", "--reflink=auto", srcisopath, newIso)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.Wrapf(err, "copying iso")
	}
	// Make it writable so we can modify it
	if err := os.Chmod(newIso, 0644); err != nil {
		return nil, errors.Wrapf(err, "setting permissions on iso")
	}
	srcisopath = newIso

	var metalimg string
	if inst.Native4k {
		metalimg = inst.CosaBuild.Meta.BuildArtifacts.Metal4KNative.Path
	} else {
		metalimg = inst.CosaBuild.Meta.BuildArtifacts.Metal.Path
	}
	metalname, err := setupMetalImage(builddir, metalimg, tempdir)
	if err != nil {
		return nil, errors.Wrapf(err, "setting up metal image")
	}

	var serializedTargetConfig string
	if offline {
		// note we leave ImageURL empty here; offline installs should now be the
		// default!

		// we want to test that a full offline install works; that includes the
		// final installed host booting offline
		serializedTargetConfig = inst.ignition.String()
	} else {
		mux := http.NewServeMux()
		mux.Handle("/", http.FileServer(http.Dir(tempdir)))
		listener, err := net.Listen("tcp", ":0")
		if err != nil {
			return nil, err
		}
		port := listener.Addr().(*net.TCPAddr).Port
		//nolint // Yeah this leaks
		go func() {
			http.Serve(listener, mux)
		}()
		baseurl := fmt.Sprintf("http://%s:%d", defaultQemuHostIPv4, port)

		// This is subtle but: for the minimal case, while we need networking to fetch the
		// rootfs, the primary install flow will still rely on osmet. So let's keep ImageURL
		// empty to exercise that path. In the future, this could be a separate scenario
		// (likely we should drop the "offline" naming and have a "remote" tag on the
		// opposite scenarios instead which fetch the metal image, so then we'd have
		// "[min]iso-install" and "[min]iso-remote-install").
		if !minimal {
			installerConfig.ImageURL = fmt.Sprintf("%s/%s", baseurl, metalname)
		}

		if minimal {
			minisopath := filepath.Join(tempdir, "minimal.iso")
			// This is obviously also available in the build dir, but to be realistic,
			// let's take it from --rootfs-output
			rootfs_path := filepath.Join(tempdir, "rootfs.img")
			// Ideally we'd use the coreos-installer of the target build here, because it's part
			// of the test workflow, but that's complex... Sadly, probably easiest is to spin up
			// a VM just to get the minimal ISO.
			cmd := exec.Command("coreos-installer", "iso", "extract", "minimal-iso", srcisopath,
				minisopath, "--output-rootfs", rootfs_path, "--rootfs-url", baseurl+"/rootfs.img")
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return nil, errors.Wrapf(err, "running coreos-installer iso extract minimal")
			}
			srcisopath = minisopath
		}

		// In this case; the target config is jut a tiny wrapper that wants to
		// fetch our hosted target.ign config

		// TODO also use https://github.com/coreos/coreos-installer/issues/118#issuecomment-585572952
		// when it arrives
		targetConfig, err := conf.EmptyIgnition().Render(conf.FailWarnings)
		if err != nil {
			return nil, err
		}
		targetConfig.AddConfigSource(baseurl + "/target.ign")
		serializedTargetConfig = targetConfig.String()

		// also save pointer config into the output dir for debugging
		if err := targetConfig.WriteFile(filepath.Join(outdir, "config-target-pointer.ign")); err != nil {
			return nil, err
		}
	}

	var keyfileArgs []string
	for nmName, nmContents := range inst.NmKeyfiles {
		path := filepath.Join(tempdir, nmName)
		if err := os.WriteFile(path, []byte(nmContents), 0600); err != nil {
			return nil, err
		}
		keyfileArgs = append(keyfileArgs, "--keyfile", path)
	}
	if len(keyfileArgs) > 0 {

		args := []string{"iso", "network", "embed", srcisopath}
		args = append(args, keyfileArgs...)
		cmd = exec.Command("coreos-installer", args...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return nil, errors.Wrapf(err, "running coreos-installer iso network embed")
		}

		installerConfig.CopyNetwork = true

		// force networking on in the initrd to verify the keyfile was used
		inst.kargs = append(inst.kargs, "rd.neednet=1")
	}

	if len(inst.kargs) > 0 {
		args := []string{"iso", "kargs", "modify", srcisopath}
		for _, karg := range inst.kargs {
			args = append(args, "--append", karg)
		}
		cmd = exec.Command("coreos-installer", args...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return nil, errors.Wrapf(err, "running coreos-installer iso kargs")
		}
	}

	if inst.Insecure {
		installerConfig.Insecure = true
	}

	installerConfigData, err := yaml.Marshal(installerConfig)
	if err != nil {
		return nil, err
	}
	mode := 0644

	inst.liveIgnition.AddSystemdUnit("boot-started.service", bootStartedUnit, conf.Enable)
	inst.liveIgnition.AddFile(installerConfig.IgnitionFile, serializedTargetConfig, mode)
	inst.liveIgnition.AddFile("/etc/coreos/installer.d/mantle.yaml", string(installerConfigData), mode)
	inst.liveIgnition.AddAutoLogin()

	if inst.MultiPathDisk {
		inst.liveIgnition.AddSystemdUnit("coreos-installer-multipath.service", `[Unit]
Description=TestISO Enable Multipath
Before=multipathd.service
DefaultDependencies=no
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/mpathconf --enable
[Install]
WantedBy=coreos-installer.target`, conf.Enable)
		inst.liveIgnition.AddSystemdUnitDropin("coreos-installer.service", "wait-for-mpath-target.conf", `[Unit]
Requires=dev-mapper-mpatha.device
After=dev-mapper-mpatha.device`)
	}

	qemubuilder := inst.Builder
	bootStartedChan, err := qemubuilder.VirtioChannelRead("bootstarted")
	if err != nil {
		return nil, err
	}

	qemubuilder.SetConfig(&inst.liveIgnition)

	// also save live config into the output dir for debugging
	liveConfigPath := filepath.Join(outdir, "config-live.ign")
	if err := inst.liveIgnition.WriteFile(liveConfigPath); err != nil {
		return nil, err
	}

	if err := qemubuilder.AddIso(srcisopath, "bootindex=3", false); err != nil {
		return nil, err
	}

	// With the recent change to use qemu -nodefaults (bc68d7c) we need to
	// request network. Otherwise we get no network devices.
	if !offline {
		qemubuilder.UsermodeNetworking = true
	}

	qinst, err := qemubuilder.Exec()
	if err != nil {
		return nil, err
	}
	cleanupTempdir = false // Transfer ownership
	instmachine := InstalledMachine{
		QemuInst: qinst,
		Tempdir:  tempdir,
	}
	switchBootOrderSignal(qinst, bootStartedChan, &instmachine.BootStartedErrorChannel)
	return &instmachine, nil
}
