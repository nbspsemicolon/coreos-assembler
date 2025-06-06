// Copyright 2025 Red Hat
// Copyright 2018 CoreOS, Inc.
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
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	cmdCreateGalleryImage = &cobra.Command{
		Use:     "create-gallery-image",
		Short:   "Create Azure Gallery image",
		Long:    "Create Azure Gallery image from a blob url",
		RunE:    runCreateGalleryImage,
		Aliases: []string{"create-gallery-image-arm"},

		SilenceUsage: true,
	}

	galleryImageName string
	galleryName      string
	architecture     string
)

func init() {
	sv := cmdCreateGalleryImage.Flags().StringVar

	sv(&galleryImageName, "gallery-image-name", "", "gallery image name")
	sv(&galleryName, "gallery-name", "kola", "gallery name")
	sv(&blobUrl, "image-blob", "", "source blob url")
	sv(&resourceGroup, "resource-group", "kola", "resource group name")
	sv(&architecture, "arch", "", "The target architecture for the image")

	Azure.AddCommand(cmdCreateGalleryImage)
}

func runCreateGalleryImage(cmd *cobra.Command, args []string) error {
	if blobUrl == "" {
		fmt.Fprintf(os.Stderr, "must supply --image-blob\n")
		os.Exit(1)
	}

	if err := api.SetupClients(); err != nil {
		fmt.Fprintf(os.Stderr, "setting up clients: %v\n", err)
		os.Exit(1)
	}

	img, err := api.CreateImage(galleryImageName, resourceGroup, blobUrl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Couldn't create Azure image: %v\n", err)
		os.Exit(1)
	}
	if img.ID == nil {
		fmt.Fprintf(os.Stderr, "received nil image\n")
		os.Exit(1)
	}
	sourceImageId := *img.ID

	galleryImage, err := api.CreateGalleryImage(galleryImageName, galleryName, resourceGroup, sourceImageId, architecture)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Couldn't create Azure Shared Image Gallery image: %v\n", err)
		os.Exit(1)
	}
	if galleryImage.ID == nil {
		fmt.Fprintf(os.Stderr, "received nil gallery image\n")
		os.Exit(1)
	}
	err = json.NewEncoder(os.Stdout).Encode(&struct {
		ID       *string
		Location *string
	}{
		ID:       galleryImage.ID,
		Location: galleryImage.Location,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Couldn't encode result: %v\n", err)
		os.Exit(1)
	}
	return nil
}
