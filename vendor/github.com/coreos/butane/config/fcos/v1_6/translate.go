// Copyright 2020 Red Hat, Inc
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
// limitations under the License.)

package v1_6

import (
	"fmt"
	"strings"

	baseutil "github.com/coreos/butane/base/util"
	"github.com/coreos/butane/config/common"
	cutil "github.com/coreos/butane/config/util"
	"github.com/coreos/butane/translate"

	"github.com/coreos/ignition/v2/config/util"
	"github.com/coreos/ignition/v2/config/v3_5/types"
	"github.com/coreos/vcontext/path"
	"github.com/coreos/vcontext/report"
)

const (
	reservedTypeGuid = "8DA63339-0007-60C0-C436-083AC8230908"
	biosTypeGuid     = "21686148-6449-6E6F-744E-656564454649"
	prepTypeGuid     = "9E1A2D38-C612-4316-AA26-8B49521E5A8B"
	espTypeGuid      = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"

	// The partition layout implemented in this file replicates
	// the layout of the OS image defined in:
	// https://github.com/coreos/coreos-assembler/blob/main/src/create_disk.sh
	//
	// It's not critical that we match that layout exactly; the hard
	// constraints are:
	//   - The desugared partition cannot be smaller than the one it
	//     replicates
	//   - The new BIOS-BOOT partition (and maybe the PReP one?) must be
	//     at the same offset as the original
	//
	// Do not change these constants!  New partition layouts must be
	// encoded into new layout templates.
	reservedV1SizeMiB = 1
	biosV1SizeMiB     = 1
	prepV1SizeMiB     = 4
	espV1SizeMiB      = 127
	bootV1SizeMiB     = 384
)

// Return FieldFilters for this spec.
func (c Config) FieldFilters() *cutil.FieldFilters {
	return nil
}

// ToIgn3_5Unvalidated translates the config to an Ignition config.  It also
// returns the set of translations it did so paths in the resultant config
// can be tracked back to their source in the source config.  No config
// validation is performed on input or output.
func (c Config) ToIgn3_5Unvalidated(options common.TranslateOptions) (types.Config, translate.TranslationSet, report.Report) {
	ret, ts, r := c.Config.ToIgn3_5Unvalidated(options)
	if r.IsFatal() {
		return types.Config{}, translate.TranslationSet{}, r
	}
	r.Merge(c.processBootDevice(&ret, &ts, options))
	for i, disk := range ret.Storage.Disks {
		// In the boot_device.mirror case, nothing specifies partition numbers
		// so match existing partitions only when `wipeTable` is false
		if !util.IsTrue(disk.WipeTable) {
			for j, partition := range disk.Partitions {
				// check for reserved partlabels
				if partition.Label != nil {
					if (*partition.Label == "BIOS-BOOT" && partition.Number != 1) || (*partition.Label == "PowerPC-PReP-boot" && partition.Number != 1) || (*partition.Label == "EFI-SYSTEM" && partition.Number != 2) || (*partition.Label == "boot" && partition.Number != 3) || (*partition.Label == "root" && partition.Number != 4) {
						r.AddOnWarn(path.New("json", "storage", "disks", i, "partitions", j, "label"), common.ErrWrongPartitionNumber)
					}
				}
			}
		}
	}

	retp, tsp, rp := c.handleUserGrubCfg(options)
	retConfig, ts := baseutil.MergeTranslatedConfigs(retp, tsp, ret, ts)
	ret = retConfig.(types.Config)
	r.Merge(rp)
	return ret, ts, r
}

// ToIgn3_5 translates the config to an Ignition config.  It returns a
// report of any errors or warnings in the source and resultant config.  If
// the report has fatal errors or it encounters other problems translating,
// an error is returned.
func (c Config) ToIgn3_5(options common.TranslateOptions) (types.Config, report.Report, error) {
	cfg, r, err := cutil.Translate(c, "ToIgn3_5Unvalidated", options)
	return cfg.(types.Config), r, err
}

// ToIgn3_5Bytes translates from a v1.6 Butane config to a v3.5.0 Ignition config. It returns a report of any errors or
// warnings in the source and resultant config. If the report has fatal errors or it encounters other problems
// translating, an error is returned.
func ToIgn3_5Bytes(input []byte, options common.TranslateBytesOptions) ([]byte, report.Report, error) {
	return cutil.TranslateBytes(input, &Config{}, "ToIgn3_5", options)
}

func (c Config) processBootDevice(config *types.Config, ts *translate.TranslationSet, options common.TranslateOptions) report.Report {
	var rendered types.Config
	renderedTranslations := translate.NewTranslationSet("yaml", "json")
	var r report.Report

	// check for high-level features
	wantLuks := util.IsTrue(c.BootDevice.Luks.Tpm2) || len(c.BootDevice.Luks.Tang) > 0 || util.IsTrue(c.BootDevice.Luks.Cex.Enabled)
	wantMirror := len(c.BootDevice.Mirror.Devices) > 0
	if !wantLuks && !wantMirror {
		return r
	}

	// compute layout rendering options
	var wantBIOSPart bool
	var wantEFIPart bool
	var wantPRePPart bool
	layout := c.BootDevice.Layout
	switch {
	case layout == nil || *layout == "x86_64":
		wantBIOSPart = true
		wantEFIPart = true
	case *layout == "aarch64":
		wantEFIPart = true
	case *layout == "ppc64le":
		wantPRePPart = true
	case *layout == "s390x-eckd" || *layout == "s390x-virt" || *layout == "s390x-zfcp":
	default:
		// should have failed validation
		panic("unknown layout")
	}

	// mirrored root disk
	if wantMirror {
		// partition disks
		for i, device := range c.BootDevice.Mirror.Devices {
			labelIndex := len(rendered.Storage.Disks) + 1
			disk := types.Disk{
				Device:    device,
				WipeTable: util.BoolToPtr(true),
			}
			if wantBIOSPart {
				disk.Partitions = append(disk.Partitions, types.Partition{
					Label:    util.StrToPtr(fmt.Sprintf("bios-%d", labelIndex)),
					SizeMiB:  util.IntToPtr(biosV1SizeMiB),
					TypeGUID: util.StrToPtr(biosTypeGuid),
				})
			} else if wantPRePPart {
				disk.Partitions = append(disk.Partitions, types.Partition{
					Label:    util.StrToPtr(fmt.Sprintf("prep-%d", labelIndex)),
					SizeMiB:  util.IntToPtr(prepV1SizeMiB),
					TypeGUID: util.StrToPtr(prepTypeGuid),
				})
			} else {
				disk.Partitions = append(disk.Partitions, types.Partition{
					Label:    util.StrToPtr(fmt.Sprintf("reserved-%d", labelIndex)),
					SizeMiB:  util.IntToPtr(reservedV1SizeMiB),
					TypeGUID: util.StrToPtr(reservedTypeGuid),
				})
			}
			if wantEFIPart {
				disk.Partitions = append(disk.Partitions, types.Partition{
					Label:    util.StrToPtr(fmt.Sprintf("esp-%d", labelIndex)),
					SizeMiB:  util.IntToPtr(espV1SizeMiB),
					TypeGUID: util.StrToPtr(espTypeGuid),
				})
			} else {
				disk.Partitions = append(disk.Partitions, types.Partition{
					Label:    util.StrToPtr(fmt.Sprintf("reserved-%d", labelIndex)),
					SizeMiB:  util.IntToPtr(reservedV1SizeMiB),
					TypeGUID: util.StrToPtr(reservedTypeGuid),
				})
			}
			disk.Partitions = append(disk.Partitions, types.Partition{
				Label:   util.StrToPtr(fmt.Sprintf("boot-%d", labelIndex)),
				SizeMiB: util.IntToPtr(bootV1SizeMiB),
			}, types.Partition{
				Label: util.StrToPtr(fmt.Sprintf("root-%d", labelIndex)),
			})
			renderedTranslations.AddFromCommonSource(path.New("yaml", "boot_device", "mirror", "devices", i), path.New("json", "storage", "disks", len(rendered.Storage.Disks)), disk)
			rendered.Storage.Disks = append(rendered.Storage.Disks, disk)

			if wantEFIPart {
				// add ESP filesystem
				espFilesystem := types.Filesystem{
					Device:         fmt.Sprintf("/dev/disk/by-partlabel/esp-%d", labelIndex),
					Format:         util.StrToPtr("vfat"),
					Label:          util.StrToPtr(fmt.Sprintf("esp-%d", labelIndex)),
					WipeFilesystem: util.BoolToPtr(true),
				}
				renderedTranslations.AddFromCommonSource(path.New("yaml", "boot_device", "mirror", "devices", i), path.New("json", "storage", "filesystems", len(rendered.Storage.Filesystems)), espFilesystem)
				rendered.Storage.Filesystems = append(rendered.Storage.Filesystems, espFilesystem)
			}
		}
		renderedTranslations.AddTranslation(path.New("yaml", "boot_device", "mirror", "devices"), path.New("json", "storage", "disks"))

		// create RAIDs
		raidDevices := func(labelPrefix string) []types.Device {
			count := len(rendered.Storage.Disks)
			ret := make([]types.Device, count)
			for i := 0; i < count; i++ {
				ret[i] = types.Device(fmt.Sprintf("/dev/disk/by-partlabel/%s-%d", labelPrefix, i+1))
			}
			return ret
		}
		rendered.Storage.Raid = []types.Raid{{
			Devices: raidDevices("boot"),
			Level:   util.StrToPtr("raid1"),
			Name:    "md-boot",
			// put the RAID superblock at the end of the
			// partition so BIOS GRUB doesn't need to
			// understand RAID
			Options: []types.RaidOption{"--metadata=1.0"},
		}, {
			Devices: raidDevices("root"),
			Level:   util.StrToPtr("raid1"),
			Name:    "md-root",
		}}
		renderedTranslations.AddFromCommonSource(path.New("yaml", "boot_device", "mirror"), path.New("json", "storage", "raid"), rendered.Storage.Raid)

		// create boot filesystem
		bootFilesystem := types.Filesystem{
			Device:         "/dev/md/md-boot",
			Format:         util.StrToPtr("ext4"),
			Label:          util.StrToPtr("boot"),
			WipeFilesystem: util.BoolToPtr(true),
		}
		renderedTranslations.AddFromCommonSource(path.New("yaml", "boot_device", "mirror"), path.New("json", "storage", "filesystems", len(rendered.Storage.Filesystems)), bootFilesystem)
		rendered.Storage.Filesystems = append(rendered.Storage.Filesystems, bootFilesystem)
	}

	// encrypted root partition
	if wantLuks {
		var luksDevice string
		switch {
		//Luks Device for dasd and zFCP-scsi
		case layout != nil && *layout == "s390x-eckd":
			luksDevice = *c.BootDevice.Luks.Device + "2"
		case layout != nil && *layout == "s390x-zfcp":
			luksDevice = *c.BootDevice.Luks.Device + "4"
		case wantMirror:
			luksDevice = "/dev/md/md-root"
		default:
			luksDevice = "/dev/disk/by-partlabel/root"
		}
		if util.IsTrue(c.BootDevice.Luks.Cex.Enabled) {
			cex, ts2, r2 := translateBootDeviceLuksCex(c.BootDevice.Luks, options)
			rendered.Storage.Luks = []types.Luks{{
				Cex:        cex,
				Device:     &luksDevice,
				Discard:    c.BootDevice.Luks.Discard,
				Label:      util.StrToPtr("luks-root"),
				Name:       "root",
				WipeVolume: util.BoolToPtr(true),
			}}
			lpath := path.New("yaml", "boot_device", "luks")
			rpath := path.New("json", "storage", "luks", 0)
			renderedTranslations.Merge(ts2.PrefixPaths(lpath, rpath.Append("cex")))
			renderedTranslations.AddTranslation(lpath.Append("discard"), rpath.Append("discard"))
			for _, f := range []string{"device", "label", "name", "wipeVolume"} {
				renderedTranslations.AddTranslation(lpath, rpath.Append(f))
			}
			renderedTranslations.AddTranslation(lpath, rpath)
			renderedTranslations.AddTranslation(lpath, path.New("json", "storage", "luks"))
			r.Merge(r2)
		} else {
			clevis, ts2, r2 := translateBootDeviceLuks(c.BootDevice.Luks, options)
			rendered.Storage.Luks = []types.Luks{{
				Clevis:     clevis,
				Device:     &luksDevice,
				Discard:    c.BootDevice.Luks.Discard,
				Label:      util.StrToPtr("luks-root"),
				Name:       "root",
				WipeVolume: util.BoolToPtr(true),
			}}
			lpath := path.New("yaml", "boot_device", "luks")
			rpath := path.New("json", "storage", "luks", 0)
			renderedTranslations.Merge(ts2.PrefixPaths(lpath, rpath.Append("clevis")))
			renderedTranslations.AddTranslation(lpath.Append("discard"), rpath.Append("discard"))
			for _, f := range []string{"device", "label", "name", "wipeVolume"} {
				renderedTranslations.AddTranslation(lpath, rpath.Append(f))
			}
			renderedTranslations.AddTranslation(lpath, rpath)
			renderedTranslations.AddTranslation(lpath, path.New("json", "storage", "luks"))
			r.Merge(r2)
		}
	}

	// create root filesystem
	var rootDevice string
	switch {
	case wantLuks:
		// LUKS, or LUKS on RAID
		rootDevice = "/dev/mapper/root"
	case wantMirror:
		// RAID without LUKS
		rootDevice = "/dev/md/md-root"
	default:
		panic("can't happen")
	}
	rootFilesystem := types.Filesystem{
		Device:         rootDevice,
		Format:         util.StrToPtr("xfs"),
		Label:          util.StrToPtr("root"),
		WipeFilesystem: util.BoolToPtr(true),
	}
	renderedTranslations.AddFromCommonSource(path.New("yaml", "boot_device"), path.New("json", "storage", "filesystems", len(rendered.Storage.Filesystems)), rootFilesystem)
	renderedTranslations.AddTranslation(path.New("yaml", "boot_device"), path.New("json", "storage", "filesystems"))
	rendered.Storage.Filesystems = append(rendered.Storage.Filesystems, rootFilesystem)

	// merge with translated config
	renderedTranslations.AddTranslation(path.New("yaml", "boot_device"), path.New("json", "storage"))
	retConfig, retTranslations := baseutil.MergeTranslatedConfigs(rendered, renderedTranslations, *config, *ts)
	*config = retConfig.(types.Config)
	*ts = retTranslations
	return r
}

func translateBootDeviceLuks(from BootDeviceLuks, options common.TranslateOptions) (to types.Clevis, tm translate.TranslationSet, r report.Report) {
	tr := translate.NewTranslator("yaml", "json", options)
	// Discard field is handled by the caller because it doesn't go
	// into types.Clevis
	tm, r = translate.Prefixed(tr, "tang", &from.Tang, &to.Tang)
	translate.MergeP(tr, tm, &r, "threshold", &from.Threshold, &to.Threshold)
	translate.MergeP(tr, tm, &r, "tpm2", &from.Tpm2, &to.Tpm2)
	// we're being called manually, not via the translate package's
	// custom translator mechanism, so we have to add the base
	// translation ourselves
	tm.AddTranslation(path.New("yaml"), path.New("json"))
	return
}

func translateBootDeviceLuksCex(from BootDeviceLuks, options common.TranslateOptions) (to types.Cex, tm translate.TranslationSet, r report.Report) {
	tr := translate.NewTranslator("yaml", "json", options)
	// Discard field is handled by the caller because it doesn't go
	// into types.Cex
	tm, r = translate.Prefixed(tr, "enabled", &from.Cex.Enabled, &to.Enabled)
	translate.MergeP(tr, tm, &r, "enabled", &from.Cex.Enabled, &to.Enabled)
	// we're being called manually, not via the translate package's
	// custom translator mechanism, so we have to add the base
	// translation ourselves
	tm.AddTranslation(path.New("yaml"), path.New("json"))
	return
}

func (c Config) handleUserGrubCfg(options common.TranslateOptions) (types.Config, translate.TranslationSet, report.Report) {
	rendered := types.Config{}
	ts := translate.NewTranslationSet("yaml", "json")
	var r report.Report
	yamlPath := path.New("yaml", "grub", "users")
	if len(c.Grub.Users) == 0 {
		// No users
		return rendered, ts, r
	}

	// create boot filesystem
	rendered.Storage.Filesystems = append(rendered.Storage.Filesystems,
		types.Filesystem{
			Device: "/dev/disk/by-label/boot",
			Format: util.StrToPtr("ext4"),
			Path:   util.StrToPtr("/boot"),
		})

	userCfgContent := []byte(buildGrubConfig(c.Grub))
	src, compression, err := baseutil.MakeDataURL(userCfgContent, nil, !options.NoResourceAutoCompression)
	if err != nil {
		r.AddOnError(yamlPath, err)
		return rendered, ts, r
	}

	// Create user.cfg file and add it to rendered config
	rendered.Storage.Files = append(rendered.Storage.Files,
		types.File{
			Node: types.Node{
				Path: "/boot/grub2/user.cfg",
			},
			FileEmbedded1: types.FileEmbedded1{
				Append: []types.Resource{
					{
						Source:      util.StrToPtr(src),
						Compression: compression,
					},
				},
			},
		})

	ts.AddFromCommonSource(yamlPath, path.New("json", "storage"), rendered.Storage)
	return rendered, ts, r
}

func buildGrubConfig(gb Grub) string {
	// Process super users and corresponding passwords
	allUsers := []string{}
	cmds := []string{}

	for _, user := range gb.Users {
		// We have already validated that user.Name and user.PasswordHash are non-empty
		allUsers = append(allUsers, user.Name)
		// Command for setting users password
		cmds = append(cmds, fmt.Sprintf("password_pbkdf2 %s %s", user.Name, *user.PasswordHash))
	}
	superUserCmd := fmt.Sprintf("set superusers=\"%s\"\n", strings.Join(allUsers, " "))
	return "# Generated by Butane\n\n" + superUserCmd + strings.Join(cmds, "\n") + "\n"
}
