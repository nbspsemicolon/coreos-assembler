// Copyright 2025 Red Hat
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

package azure

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	cmdDeleteGalleryImage = &cobra.Command{
		Use:     "delete-gallery-image",
		Short:   "Delete Azure Gallery image",
		Long:    "Remove a Shared Image Gallery image from Azure.",
		RunE:    runDeleteGalleryImage,
		Aliases: []string{"delete-gallery-image-arm"},

		SilenceUsage: true,
	}

	deleteGallery bool
)

func init() {
	sv := cmdDeleteGalleryImage.Flags().StringVar
	bv := cmdDeleteGalleryImage.Flags().BoolVar

	sv(&imageName, "gallery-image-name", "", "gallery image name")
	sv(&resourceGroup, "resource-group", "kola", "resource group name")
	sv(&galleryName, "gallery-name", "kola", "gallery name")
	bv(&deleteGallery, "delete-entire-gallery", false, "delete entire gallery")

	Azure.AddCommand(cmdDeleteGalleryImage)
}

func runDeleteGalleryImage(cmd *cobra.Command, args []string) error {
	if err := api.SetupClients(); err != nil {
		return fmt.Errorf("setting up clients: %v\n", err)
	}

	if deleteGallery {
		err := api.DeleteGallery(galleryName, resourceGroup)
		if err != nil {
			return fmt.Errorf("Couldn't delete gallery: %v\n", err)
		}
		plog.Printf("Gallery %q in resource group %q removed", galleryName, resourceGroup)
		return nil
	}

	err := api.DeleteGalleryImage(imageName, resourceGroup, galleryName)
	if err != nil {
		return fmt.Errorf("Couldn't delete gallery image: %v\n", err)
	}

	// Gallery image versions are backed by managed images with the same name,
	// so we can easily identify and delete them together.
	err = api.DeleteImage(imageName, resourceGroup)
	if err != nil {
		return fmt.Errorf("Couldn't delete image: %v\n", err)
	}

	plog.Printf("Image %q in gallery %q in resource group %q removed", imageName, galleryName, resourceGroup)
	return nil
}
