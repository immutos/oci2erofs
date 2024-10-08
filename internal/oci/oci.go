// SPDX-License-Identifier: AGPL-3.0-or-later
/*
 * Copyright (C) 2024 Damian Peckett <damian@pecke.tt>.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */

package oci

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/platforms"
	"github.com/dpeckett/archivefs/tarfs"
	"github.com/dpeckett/uncompr"
	"github.com/immutos/oci2erofs/internal/overlayfs"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

// LoadImage loads an OCI image from the given imageFS, ref, and platform.
// It returns an overlayfs.FS of the image's root filesystem, a function to
// close the image, and an error if any.
func LoadImage(tempDir string, imageFS fs.FS, ref string, platform *ocispecs.Platform) (fs.FS, func() error, error) {
	if err := verifyImageLayoutVersion(imageFS); err != nil {
		return nil, nil, err
	}

	manifest, err := manifestForRef(imageFS, ref, platform)
	if err != nil {
		return nil, nil, err
	}

	var layers []fs.FS
	var closers []func() error

	for _, layerDescriptor := range manifest.Layers {
		layerPath := filepath.Join("blobs", string(layerDescriptor.Digest.Algorithm()), layerDescriptor.Digest.Encoded())
		layer, close, err := loadLayer(tempDir, imageFS, layerPath)
		if err != nil {
			return nil, nil, err
		}

		layers = append(layers, layer)
		closers = append(closers, close)
	}

	closeAll := func() error {
		for _, close := range closers {
			if err := close(); err != nil {
				return err
			}
		}
		return nil
	}

	rootFS, err := overlayfs.New(layers)
	if err != nil {
		_ = closeAll()
		return nil, nil, fmt.Errorf("failed to create overlayfs: %w", err)
	}

	return rootFS, closeAll, nil
}

func loadLayer(tempDir string, imageFS fs.FS, layerPath string) (fs.FS, func() error, error) {
	f, err := imageFS.Open(layerPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open layer: %w", err)
	}
	defer f.Close()

	dr, err := uncompr.NewReader(f)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create decompressing reader: %w", err)
	}
	defer dr.Close()

	decompressedLayerPath := filepath.Join(tempDir, filepath.Base(layerPath)+".tar")
	decompressedLayerFile, err := os.OpenFile(decompressedLayerPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create temporary tar file: %w", err)
	}

	if _, err := io.Copy(decompressedLayerFile, dr); err != nil {
		return nil, nil, fmt.Errorf("failed to decompress layer: %w", err)
	}

	fsys, err := tarfs.Open(decompressedLayerFile)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open decompressed layer: %w", err)
	}

	return fsys, decompressedLayerFile.Close, nil
}

func manifestForRef(imageFS fs.FS, ref string, platform *ocispecs.Platform) (*ocispecs.Manifest, error) {
	indexFile, err := imageFS.Open("index.json")
	if err != nil {
		return nil, fmt.Errorf("failed to open index: %w", err)
	}
	defer indexFile.Close()

	var index ocispecs.Index
	if err := json.NewDecoder(indexFile).Decode(&index); err != nil {
		return nil, fmt.Errorf("failed to unmarshal index: %w", err)
	}

	if len(index.Manifests) == 0 {
		return nil, errors.New("no manifests found")
	}

	var manifestDescriptor *ocispecs.Descriptor
	if ref == "" {
		if len(index.Manifests) > 1 {
			return nil, errors.New("multiple manifests found, ref must be specified")
		}

		manifestDescriptor = &index.Manifests[0]
	} else {
		for _, desc := range index.Manifests {
			if desc.Annotations[ocispecs.AnnotationRefName] == ref {
				desc := desc
				manifestDescriptor = &desc
				break
			}
		}
	}
	if manifestDescriptor == nil {
		return nil, fmt.Errorf("no manifest found for ref %s", ref)
	}

	if manifestDescriptor.MediaType == ocispecs.MediaTypeImageIndex {
		imageIndexPath := filepath.Join("blobs", string(manifestDescriptor.Digest.Algorithm()), manifestDescriptor.Digest.Encoded())

		imageIndexFile, err := imageFS.Open(imageIndexPath)
		if err != nil {
			return nil, fmt.Errorf("failed to open image index file: %w", err)
		}
		defer imageIndexFile.Close()

		var imageIndex ocispecs.Index
		if err := json.NewDecoder(imageIndexFile).Decode(&imageIndex); err != nil {
			return nil, fmt.Errorf("failed to unmarshal image index: %w", err)
		}

		// Find the manifest for the platform.
		manifestDescriptor = nil
		if platform == nil {
			if len(imageIndex.Manifests) > 0 {
				manifestDescriptor = &imageIndex.Manifests[0]
			}
		} else {
			for _, desc := range imageIndex.Manifests {
				if platforms.NewMatcher(*platform).Match(*desc.Platform) {
					desc := desc
					manifestDescriptor = &desc
					break
				}
			}
		}

		if manifestDescriptor == nil {
			return nil, fmt.Errorf("no manifest found for platform %s", platforms.Format(*platform))
		}
	} else if manifestDescriptor.MediaType == ocispecs.MediaTypeImageManifest {
		// Check if the platform is correct.
		if platform != nil && !platforms.NewMatcher(*platform).Match(*manifestDescriptor.Platform) {
			return nil, errors.New("platform is not present in image")
		}
	} else {
		return nil, fmt.Errorf("unexpected manifest media type: %s", manifestDescriptor.MediaType)
	}

	manifestPath := filepath.Join("blobs", string(manifestDescriptor.Digest.Algorithm()), manifestDescriptor.Digest.Encoded())

	manifestFile, err := imageFS.Open(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open manifest file: %w", err)
	}
	defer manifestFile.Close()

	var manifest ocispecs.Manifest
	if err := json.NewDecoder(manifestFile).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("failed to unmarshal manifest: %w", err)
	}

	return &manifest, nil
}

func verifyImageLayoutVersion(imageFS fs.FS) error {
	ociLayoutFile, err := imageFS.Open("oci-layout")
	if err != nil {
		return fmt.Errorf("failed to open oci-layout: %w", err)
	}
	defer ociLayoutFile.Close()

	var ociLayout struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	if err := json.NewDecoder(ociLayoutFile).Decode(&ociLayout); err != nil {
		return fmt.Errorf("failed to unmarshal oci-layout: %w", err)
	}

	if ociLayout.ImageLayoutVersion != ocispecs.ImageLayoutVersion {
		return fmt.Errorf("unsupported image layout version: %s", ociLayout.ImageLayoutVersion)
	}

	return nil
}
